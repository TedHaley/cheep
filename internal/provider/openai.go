package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

// DiscoverModels probes an endpoint for its model list. The caller passes the
// access details only (endpoint + key); cheep figures out the rest. It tries the
// endpoint as-given and with a "/v1" suffix, and returns the base that actually
// responded plus the model IDs it serves.
func DiscoverModels(rawBase, apiKey string) (resolvedBase string, models []string, err error) {
	rawBase = strings.TrimRight(rawBase, "/")
	candidates := []string{rawBase}
	if !strings.HasSuffix(rawBase, "/v1") {
		candidates = append(candidates, rawBase+"/v1")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var lastErr error
	for _, base := range candidates {
		req, reqErr := http.NewRequest("GET", base+"/models", nil)
		if reqErr != nil {
			lastErr = reqErr
			continue
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s/models -> HTTP %d", base, resp.StatusCode)
			continue
		}
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if jErr := json.Unmarshal(data, &parsed); jErr != nil {
			lastErr = jErr
			continue
		}
		var ids []string
		for _, m := range parsed.Data {
			ids = append(ids, m.ID)
		}
		if len(ids) == 0 {
			// Some servers return 200 with an error body on the wrong path;
			// don't accept an empty list, keep probing other candidates.
			lastErr = fmt.Errorf("%s/models returned no models", base)
			continue
		}
		return base, ids, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no models endpoint responded")
	}
	return "", nil, lastErr
}

// OpenAI talks to any OpenAI-compatible chat-completions endpoint over raw HTTP.
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

func (o *OpenAI) Complete(ctx context.Context, model, system string, messages []core.Message, tools []core.Tool) (core.Turn, error) {
	model, level := core.SplitThinking(model)
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
	// A ":low|:medium|:high" model suffix maps to reasoning_effort. Servers
	// that don't know the field generally ignore it; it is only sent when the
	// user opted in via the suffix.
	if level != "" && level != "off" {
		body["reasoning_effort"] = level
	}
	if len(nativeTools) > 0 {
		body["tools"] = nativeTools
	}
	buf, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", o.BaseURL+"/chat/completions", bytes.NewReader(buf))
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
