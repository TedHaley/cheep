package provider

import (
	"bytes"
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

func (a *Anthropic) Complete(model, system string, messages []core.Message, tools []core.Tool) (core.Turn, error) {
	var nativeTools []map[string]any
	for _, t := range tools {
		nativeTools = append(nativeTools, map[string]any{
			"name": t.Name, "description": t.Description, "input_schema": t.Parameters,
		})
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": a.MaxTokens,
		"system":     system,
		"messages":   a.toNative(messages),
	}
	if len(nativeTools) > 0 {
		body["tools"] = nativeTools
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", a.BaseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return core.Turn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

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
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
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
	return core.Turn{
		Message:      msg,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}
