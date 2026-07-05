package config

import "testing"

func init() {
	// Simulate the pricing hook main wires up: a known cloud model resolves,
	// an unknown/local one does not.
	WindowLookup = func(model string) (int, bool) {
		if model == "cloud-known" {
			return 128000, true
		}
		return 0, false
	}
}

func TestContextWindowDerivesBudgets(t *testing.T) {
	c := Config{
		Orchestrator: Agent{Provider: "openai", Model: "big", ContextWindow: 200000},
		Executors: []Agent{
			{Name: "small", Provider: "openai", Model: "qwen", ContextWindow: 8000},
			{Name: "nowin", Provider: "openai", Model: "m"}, // unknown window → legacy defaults
		},
	}
	c.ApplyDefaults()

	if got := c.Orchestrator.ContextBudget; got != 150000 { // 75% of 200k
		t.Errorf("orch ContextBudget = %d, want 150000", got)
	}
	small := c.Executors[0]
	if small.ContextBudget != 6000 { // 75% of 8k: compact well before the window
		t.Errorf("small ContextBudget = %d, want 6000", small.ContextBudget)
	}
	if small.TokenBudget != 7600 { // 95% of 8k: hard stop just under the window
		t.Errorf("small TokenBudget = %d, want 7600", small.TokenBudget)
	}
	if small.ContextBudget >= small.TokenBudget {
		t.Error("compaction must trigger before the hard stop")
	}
	// A known cloud model with no explicit window auto-fills from the hook.
	auto := Config{Orchestrator: Agent{Provider: "anthropic", Model: "cloud-known"}}
	auto.ApplyDefaults()
	if auto.Orchestrator.ContextWindow != 128000 || auto.Orchestrator.ContextBudget != 96000 {
		t.Errorf("auto-fill: window=%d budget=%d, want 128000/96000",
			auto.Orchestrator.ContextWindow, auto.Orchestrator.ContextBudget)
	}

	if nowin := c.Executors[1]; nowin.TokenBudget != 100000 || nowin.ContextBudget != 0 {
		t.Errorf("unknown-window executor should keep legacy defaults, got token=%d ctx=%d",
			nowin.TokenBudget, nowin.ContextBudget)
	}
}
