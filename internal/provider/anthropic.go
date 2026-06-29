package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

// Anthropic talks to the Claude Messages API over raw HTTP (no SDK).
type Anthropic struct {
	APIKey    string
	BaseURL   string
	MaxTokens int
	client    *http.Client
}

func NewAnthropic(apiKey string, maxTokens int) *Anthropic {
	return &Anthropic{
		APIKey:    apiKey,
		BaseURL:   "https://api.anthropic.com",
		MaxTokens: maxTokens,
		client:    &http.Client{Timeout: 600 * time.Second},
	}
}

type anthMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func (a *Anthropic) toNative(messages []core.Message) []anthMessage {
	var out []anthMessage
	for _, m := range messages {
		switch m.Role {
		case "user":
			out = append(out, anthMessage{Role: "user", Content: m.Text})
		case "assistant":
			var content []map[string]any
			if m.Text != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Text})
			}
			for _, tc := range m.ToolCalls {
				content = append(content, map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": tc.Arguments,
				})
			}
			out = append(out, anthMessage{Role: "assistant", Content: content})
		case "tool":
			block := map[string]any{
				"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Text,
			}
			// Multiple tool results after one assistant turn merge into one user turn.
			if n := len(out); n > 0 && out[n-1].Role == "user" {
				if arr, ok := out[n-1].Content.([]map[string]any); ok {
					out[n-1].Content = append(arr, block)
					continue
				}
			}
			out = append(out, anthMessage{Role: "user", Content: []map[string]any{block}})
		}
	}
	return out
}

var ephemeral = map[string]any{"type": "ephemeral"}

// markLastCacheable puts a cache breakpoint on the last block of the last
// message, so the whole conversation prefix up to here is cached and reused next
// turn. A user message stored as a plain string is promoted to a text block.
func markLastCacheable(msgs []anthMessage) {
	if len(msgs) == 0 {
		return
	}
	i := len(msgs) - 1
	switch c := msgs[i].Content.(type) {
	case string:
		if c != "" {
			msgs[i].Content = []map[string]any{{"type": "text", "text": c, "cache_control": ephemeral}}
		}
	case []map[string]any:
		if len(c) > 0 {
			c[len(c)-1]["cache_control"] = ephemeral
		}
	}
}

func (a *Anthropic) Complete(ctx context.Context, model, system string, messages []core.Message, tools []core.Tool) (core.Turn, error) {
	var nativeTools []map[string]any
	for _, t := range tools {
		nativeTools = append(nativeTools, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": t.Parameters,
		})
	}
	native := a.toNative(messages)
	// Prompt caching: cache the static prefixes (system + tools) and the
	// conversation so far. Each cached prefix is reused on the next turn at a
	// fraction of the input cost — a big saving across an agent's many turns.
	markLastCacheable(native)
	body := map[string]any{
		"model":      model,
		"max_tokens": a.MaxTokens,
		"messages":   native,
	}
	if system != "" {
		body["system"] = []map[string]any{{"type": "text", "text": system, "cache_control": ephemeral}}
	} else {
		body["system"] = system
	}
	if len(nativeTools) > 0 {
		nativeTools[len(nativeTools)-1]["cache_control"] = ephemeral
		body["tools"] = nativeTools
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return core.Turn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	resp, err := a.client.Do(req)
	if err != nil {
		return core.Turn{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return core.Turn{}, fmt.Errorf("anthropic %d: %s", resp.StatusCode, string(data))
	}

	var parsed struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return core.Turn{}, err
	}

	msg := core.Message{Role: "assistant"}
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			msg.Text += b.Text
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls,
				core.ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		}
	}
	// Count fresh input + cache writes as input tokens (writes are ~full price);
	// cache reads (~0.1x) are omitted, so the cost meter reflects caching savings
	// when a prefix is reused across turns.
	return core.Turn{
		Message:      msg,
		InputTokens:  parsed.Usage.InputTokens + parsed.Usage.CacheCreationTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}
