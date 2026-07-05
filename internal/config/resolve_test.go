package config

import "testing"

func TestResolveWindowsProbesLocalModels(t *testing.T) {
	c := Config{
		Orchestrator: Agent{Provider: "anthropic", Model: "claude"}, // skipped: anthropic
		Executors: []Agent{
			{Name: "local", Provider: "openai", Endpoint: "http://127.0.0.1:1234/v1", Model: "qwen"},
			{Name: "explicit", Provider: "openai", Endpoint: "http://x/v1", Model: "m", ContextWindow: 4096},
		},
	}
	c.ApplyDefaults() // local has no window yet → legacy 100k token budget

	probe := func(endpoint, key, model string) (int, bool) {
		if model == "qwen" {
			return 262144, true // what LM Studio reports loaded
		}
		return 0, false
	}
	c.ResolveWindows(probe)

	local := c.Executors[0]
	if local.ContextWindow != 262144 {
		t.Errorf("local window = %d, want 262144", local.ContextWindow)
	}
	if local.ContextBudget != 262144*3/4 {
		t.Errorf("local ContextBudget = %d, want %d (compact at 75%%)", local.ContextBudget, 262144*3/4)
	}
	if local.TokenBudget != 262144*95/100 {
		t.Errorf("local TokenBudget = %d, want %d (stop at 95%%, not the legacy 100k)", local.TokenBudget, 262144*95/100)
	}
	// an explicit window is never overwritten by the probe
	if c.Executors[1].ContextWindow != 4096 {
		t.Errorf("explicit window was overwritten: %d", c.Executors[1].ContextWindow)
	}
}
