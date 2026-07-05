package orchestrator

import (
	"strings"
	"testing"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/pricing"
)

func TestEscalateTarget(t *testing.T) {
	// Ladder: local(0) < deepseek(1.38) < sonnet(18) < opus(90).
	order := []string{"local", "deepseek", "sonnet", "opus"}
	score := map[string]float64{"local": 0, "deepseek": 1.38, "sonnet": 18, "opus": 90}
	usable := map[string]bool{"local": true, "deepseek": true, "sonnet": true, "opus": true}

	// From local, the next rung up is the cheapest pricier one: deepseek.
	if got := escalateTarget(order, score, usable, "local", map[string]bool{"local": true}); got != "deepseek" {
		t.Fatalf("local -> %q, want deepseek", got)
	}
	// From deepseek with both cheaper tried, escalate to sonnet (not opus).
	if got := escalateTarget(order, score, usable, "deepseek",
		map[string]bool{"local": true, "deepseek": true}); got != "sonnet" {
		t.Fatalf("deepseek -> %q, want sonnet", got)
	}
	// At the top, nothing higher.
	if got := escalateTarget(order, score, usable, "opus",
		map[string]bool{"opus": true}); got != "" {
		t.Fatalf("opus -> %q, want empty", got)
	}
	// Unusable tiers are skipped: from local, deepseek down → jump to sonnet.
	u2 := map[string]bool{"local": true, "deepseek": false, "sonnet": true, "opus": true}
	if got := escalateTarget(order, score, u2, "local", map[string]bool{"local": true}); got != "sonnet" {
		t.Fatalf("local (deepseek down) -> %q, want sonnet", got)
	}
}

func TestPricingScoreOrdering(t *testing.T) {
	local := config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1234/v1", Model: "qwen"}
	deepseek := config.Agent{Provider: "openai", Endpoint: "https://api.deepseek.com/v1", Model: "deepseek-chat"}
	sonnet := config.Agent{Provider: "anthropic", Model: "claude-sonnet-4-6"}
	opus := config.Agent{Provider: "anthropic", Model: "claude-opus-4-1"}
	override := config.Agent{Provider: "openai", Endpoint: "https://x", Model: "mystery", PriceIn: 1, PriceOut: 1}

	if s := pricing.Score(local); s != 0 {
		t.Fatalf("local score = %v, want 0", s)
	}
	if !(pricing.Score(local) < pricing.Score(deepseek) &&
		pricing.Score(deepseek) < pricing.Score(sonnet) &&
		pricing.Score(sonnet) < pricing.Score(opus)) {
		t.Fatalf("ordering wrong: local=%v deepseek=%v sonnet=%v opus=%v",
			pricing.Score(local), pricing.Score(deepseek), pricing.Score(sonnet), pricing.Score(opus))
	}
	if s := pricing.Score(override); s != 2 {
		t.Fatalf("override score = %v, want 2 (price_in+price_out)", s)
	}
}

func TestRosterCostAware(t *testing.T) {
	execs := []config.Agent{
		{Name: "claude", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		{Name: "local", Provider: "openai", Endpoint: "http://127.0.0.1:1234/v1", Model: "qwen"},
		{Name: "deepseek", Provider: "openai", Endpoint: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
	}
	out := roster(execs, 5)
	t.Logf("\n%s", out)
	li, di, ci := strings.Index(out, `"local"`), strings.Index(out, `"deepseek"`), strings.Index(out, `"claude"`)
	if !(li >= 0 && li < di && di < ci) {
		t.Fatalf("not cheapest-first: local=%d deepseek=%d claude=%d", li, di, ci)
	}
	if !strings.Contains(out, "$5.00") {
		t.Fatal("project budget not shown")
	}
}

func TestResolveExecutor(t *testing.T) {
	known := map[string]execRuntime{"a": {}, "b": {}}
	// No rules: unknown/empty falls back.
	if ex, err := resolveExecutor("", known, "a", false); err != nil || ex != "a" {
		t.Fatalf("%q %v", ex, err)
	}
	if ex, err := resolveExecutor("nope", known, "a", false); err != nil || ex != "a" {
		t.Fatalf("%q %v", ex, err)
	}
	// Known name always resolves.
	if ex, err := resolveExecutor("b", known, "a", true); err != nil || ex != "b" {
		t.Fatalf("%q %v", ex, err)
	}
	// Rules active: silence and typos are rejected, not defaulted.
	if _, err := resolveExecutor("", known, "a", true); err == nil {
		t.Fatal("empty executor must be rejected under rules")
	}
	if _, err := resolveExecutor("typo", known, "a", true); err == nil {
		t.Fatal("unknown executor must be rejected under rules")
	}
}

func TestParseMetric(t *testing.T) {
	cases := []struct {
		out  string
		want float64
		err  bool
	}{
		{"coverage: 73.4% of statements", 73.4, false},
		{"ok  \tpkg\t0.5s\ncoverage: 81.0%", 81.0, false},
		{"warnings: 14", 14, false},
		{"BenchmarkX-10  1200 ns/op\n-3.5", -3.5, false},
		{"no numbers here", 0, true},
	}
	for _, c := range cases {
		got, err := parseMetric(c.out)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("parseMetric(%q) = %v,%v want %v,err=%v", c.out, got, err, c.want, c.err)
		}
	}
}
