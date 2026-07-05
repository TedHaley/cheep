package tui

import (
	"testing"

	"github.com/TedHaley/cheep/internal/config"
)

// TestConfigEqualIgnoresResolvedWindows: window detection enriches the live
// config in memory; that must NOT read as a config change (which reprinted the
// banner mid-session). A real orchestrator/executor change still must.
func TestConfigEqualIgnoresResolvedWindows(t *testing.T) {
	disk := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://x/v1", Model: "qwen"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://x/v1", Model: "qwen"}},
	}
	// live = disk after ResolveWindows-style enrichment (windows/budgets filled)
	live := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://x/v1", Model: "qwen",
			ContextWindow: 262144, ContextBudget: 196608},
		Executors: []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://x/v1", Model: "qwen",
			ContextWindow: 262144, ContextBudget: 196608, TokenBudget: 248036}},
	}
	if !configEqual(disk, live) {
		t.Error("resolved windows should NOT count as a config change (banner misfire)")
	}
	// mutating the inputs must not have leaked (slice copy)
	if disk.Executors[0].ContextWindow != 0 {
		t.Error("configEqual mutated its argument")
	}

	// a genuine change is still detected
	changed := live
	changed.Orchestrator.Model = "claude-sonnet-4-6"
	if configEqual(disk, changed) {
		t.Error("a real orchestrator-model change must be detected")
	}
}
