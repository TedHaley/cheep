package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ContextWindow asks a local OpenAI-compatible server for the context length
// the model is actually serving — the real "max available" right now. Tries
// LM Studio's native API (loaded_context_length / max_context_length) and
// llama.cpp's /props (n_ctx). Returns 0 when nothing answers (e.g. a cloud
// endpoint), so callers fall back to the dataset or an explicit override.
func ContextWindow(baseURL, apiKey, model string) (int, bool) {
	root := strings.TrimRight(baseURL, "/")
	root = strings.TrimSuffix(root, "/v1") // native APIs sit beside /v1
	client := &http.Client{Timeout: 4 * time.Second}
	get := func(url string) []byte {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return b
	}

	// LM Studio: /api/v0/models lists each model with its context lengths.
	if b := get(root + "/api/v0/models"); b != nil {
		var out struct {
			Data []struct {
				ID     string `json:"id"`
				Loaded int    `json:"loaded_context_length"`
				Max    int    `json:"max_context_length"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &out) == nil {
			for _, m := range out.Data {
				if m.ID != model {
					continue
				}
				if m.Loaded > 0 {
					return m.Loaded, true
				}
				if m.Max > 0 {
					return m.Max, true
				}
			}
		}
	}

	// llama.cpp server: /props exposes the loaded n_ctx.
	if b := get(root + "/props"); b != nil {
		var p struct {
			NCtx     int `json:"n_ctx"`
			Defaults struct {
				NCtx int `json:"n_ctx"`
			} `json:"default_generation_settings"`
		}
		if json.Unmarshal(b, &p) == nil {
			if p.NCtx > 0 {
				return p.NCtx, true
			}
			if p.Defaults.NCtx > 0 {
				return p.Defaults.NCtx, true
			}
		}
	}
	return 0, false
}

// TESTPROBE — temporary
