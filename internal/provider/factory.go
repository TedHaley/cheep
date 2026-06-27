package provider

import "github.com/TedHaley/cheep/internal/core"

// For returns the right provider for a role. "anthropic" speaks the Claude
// Messages API; anything else is treated as an OpenAI-compatible endpoint.
func For(providerKind, baseURL, apiKey string, maxTokens int) core.Provider {
	if providerKind == "anthropic" {
		return NewAnthropic(apiKey, maxTokens)
	}
	return NewOpenAI(baseURL, apiKey, maxTokens)
}
