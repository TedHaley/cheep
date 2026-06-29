// Package pricing holds cheep's (rough, overridable) model cost estimates. It is
// the single source of truth for the cost meter and for ordering executors
// cheapest-first so cheap-first escalation knows which way is "up".
package pricing

import (
	"strings"

	"github.com/TedHaley/cheep/internal/config"
)

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

// Rate returns per-1M-token input/output prices for a model name from the table.
func Rate(model string) (in, out float64, known bool) {
	low := strings.ToLower(model)
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
