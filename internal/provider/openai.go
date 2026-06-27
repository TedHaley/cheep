package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

// OpenAI talks to any OpenAI-compatible chat-completions endpoint over raw HTTP:
// Ollama, vLLM, llama.cpp, etc.
type OpenAI struct {
	APIKey    string
	BaseURL   string
	MaxTokens int
	client    *http.Client
}

func NewOpenAI(baseURL, apiKey string, maxTokens int) *OpenAI {
	if apiKey == "" {
		apiKey = "not-needed"
	}
	return &OpenAI{
		APIKey:    apiKey,
		BaseURL:   strings.TrimRight(baseURL, "/"),
		MaxTokens: maxTokens,
		client:    &http.Client{Timeout: 600 * time.Second},
	}
}

func (o *OpenAI) toNative(system string, messages []core.Message) []map[string]any {
	out := []map[string]any{{"role": "system", "content": system}}
	for _, m := range messages {
		switch m.Role {
		case "user":
			out = append(out, map[string]any{"role": "user", "content": m.Text})
		case "assistant":
			msg := map[string]any{"role": "assistant"}
			if m.Text != "" {
				msg["content"] = m.Text
			} else {
				msg["content"] = nil
			}
			if len(m.ToolCalls) > 0 {
				var tcs []map[string]any
				for _, tc := range m.ToolCalls {
					argBytes, _ := json.Marshal(tc.Arguments)
					tcs = append(tcs, map[string]any{
						"id": tc.ID, "type": "function",
						"function": map[string]any{"name": tc.Name, "arguments": string(argBytes)},
					})
				}
				msg["tool_calls"] = tcs
			}
			out = append(out, msg)
		case "tool":
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": m.ToolCallID, "content": m.Text,
			})
		}
	}
	return out
}

func (o *OpenAI) Complete(model, system string, messages []core.Message, tools []core.Tool) (core.Turn, error) {
	var nativeTools []map[string]any
	for _, t := range tools {
		nativeTools = append(nativeTools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters,
			},
		})
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": o.MaxTokens,
		"messages":   o.toNative(system, messages),
	}
	if len(nativeTools) > 0 {
		body["tools"] = nativeTools
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return core.Turn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return core.Turn{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return core.Turn{}, fmt.Errorf("openai %d: %s", resp.StatusCode, string(data))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return core.Turn{}, err
	}
	if len(parsed.Choices) == 0 {
		return core.Turn{}, fmt.Errorf("openai: response had no choices")
	}

	cm := parsed.Choices[0].Message
	msg := core.Message{Role: "assistant", Text: cm.Content}
	for _, tc := range cm.ToolCalls {
		args := map[string]any{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		msg.ToolCalls = append(msg.ToolCalls,
			core.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: args})
	}
	return core.Turn{
		Message:      msg,
		InputTokens:  parsed.Usage.PromptTokens,
		OutputTokens: parsed.Usage.CompletionTokens,
	}, nil
}
