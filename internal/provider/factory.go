package provider

import "github.com/TedHaley/cheep/internal/core"

// For returns the right provider for a role. "anthropic" speaks the Claude
// Messages API; anything else is treated as an OpenAI-compatible endpoint.
// The provider is wrapped with a per-endpoint concurrency limiter (see
// limit.go), so sessions sharing one local model don't hammer it in parallel.
func For(providerKind, baseURL, apiKey string, maxTokens int) core.Provider {
	var p core.Provider
	if providerKind == "anthropic" {
		p = NewAnthropic(apiKey, maxTokens)
	} else {
		p = NewOpenAI(baseURL, apiKey, maxTokens)
	}
	return limited{inner: p, key: normEndpoint(baseURL)}
}
