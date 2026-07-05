// Package pricing holds cheep's (rough, overridable) model cost estimates. It is
// the single source of truth for the cost meter and for ordering executors
// cheapest-first so cheap-first escalation knows which way is "up".
package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

// Live prices are fetched from BerriAI/LiteLLM's maintained dataset, cached to
// ~/.cheep/prices.json, and consulted before the built-in table below.
const litellmURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

var (
	mu      sync.RWMutex
	fetched map[string][2]float64 // lower(model) -> {inputUSDper1M, outputUSDper1M}
)

func cachePath() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "prices.json"), nil
}

// Load reads the cached price dataset into memory (no network). Call at startup.
func Load() {
	if p, err := cachePath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			parseInto(b)
		}
	}
}

// MaybeRefresh fetches the dataset in the background if the cache is missing or
// older than a week. Never blocks startup.
func MaybeRefresh() {
	p, err := cachePath()
	if err != nil {
		return
	}
	if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < 7*24*time.Hour {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = Refresh(ctx)
	}()
}

// Refresh fetches the latest dataset, updates memory, and rewrites the cache.
func Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", litellmURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prices: http %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	parseInto(b)
	if p, err := cachePath(); err == nil {
		if h, err := config.Home(); err == nil {
			_ = os.MkdirAll(h, 0o700)
		}
		_ = os.WriteFile(p, b, 0o600)
	}
	return nil
}

func parseInto(b []byte) {
	var raw map[string]struct {
		In  float64 `json:"input_cost_per_token"`
		Out float64 `json:"output_cost_per_token"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	m := make(map[string][2]float64, len(raw))
	for k, v := range raw {
		if v.In > 0 || v.Out > 0 {
			m[strings.ToLower(k)] = [2]float64{v.In * 1e6, v.Out * 1e6} // per-token → per-1M
		}
	}
	if len(m) > 0 {
		mu.Lock()
		fetched = m
		mu.Unlock()
	}
}

// lookupFetched tries the live dataset: exact match, then the part after a
// provider prefix ("deepseek/deepseek-chat" → "deepseek-chat").
func lookupFetched(low string) (in, out float64, ok bool) {
	mu.RLock()
	m := fetched
	mu.RUnlock()
	if m == nil {
		return 0, 0, false
	}
	if r, ok := m[low]; ok {
		return r[0], r[1], true
	}
	if i := strings.LastIndex(low, "/"); i >= 0 {
		if r, ok := m[low[i+1:]]; ok {
			return r[0], r[1], true
		}
	}
	return 0, 0, false
}

// table maps a model-name substring to (input, output) USD per 1M tokens.
// Estimates only — drift over time; override per agent with price_in/price_out.
var table = []struct {
	key     string
	in, out float64
}{
	{"claude-opus", 15, 75},
	{"claude-sonnet", 3, 15},
	{"claude-haiku", 0.80, 4},
	{"gpt-4o-mini", 0.15, 0.60},
	{"gpt-4o", 2.50, 10},
	{"o4-mini", 1.10, 4.40},
	{"deepseek", 0.28, 1.10},
	{"grok", 2, 10},
	{"mistral", 2, 6},
}

// IsLocal reports whether an endpoint is a local server (treated as free).
func IsLocal(provider, endpoint string) bool {
	return provider != "anthropic" &&
		(strings.Contains(endpoint, "localhost") ||
			strings.Contains(endpoint, "127.0.0.1") ||
			strings.Contains(endpoint, "0.0.0.0"))
}

// Rate returns per-1M-token input/output prices for a model name, preferring the
// live LiteLLM dataset and falling back to the built-in table.
func Rate(model string) (in, out float64, known bool) {
	base, _ := core.SplitThinking(model) // "sonnet:high" prices as "sonnet"
	low := strings.ToLower(base)
	if i, o, ok := lookupFetched(low); ok {
		return i, o, true
	}
	for _, p := range table {
		if strings.Contains(low, p.key) {
			return p.in, p.out, true
		}
	}
	return 0, 0, false
}

// AgentRate returns an agent's price and a kind: "local", "priced", or "unknown".
func AgentRate(a config.Agent) (in, out float64, kind string) {
	if IsLocal(a.Provider, a.Endpoint) {
		return 0, 0, "local"
	}
	if a.PriceIn > 0 || a.PriceOut > 0 {
		return a.PriceIn, a.PriceOut, "priced"
	}
	if i, o, ok := Rate(a.Model); ok {
		return i, o, "priced"
	}
	return 0, 0, "unknown"
}

// Cost is the USD cost of in/out tokens at the given per-1M rates.
func Cost(inTok, outTok int, inR, outR float64) float64 {
	return float64(inTok)*inR/1e6 + float64(outTok)*outR/1e6
}

// Score is a blended $/1M-token cost used to order executors cheapest-first for
// escalation. Local = 0; unknown cloud sorts in the middle so a cheap-but-unpriced
// endpoint is tried before a known-expensive one.
func Score(a config.Agent) float64 {
	in, out, kind := AgentRate(a)
	switch kind {
	case "local":
		return 0
	case "unknown":
		return 5
	default:
		return in + out
	}
}
