package core

import "testing"

func TestSplitThinking(t *testing.T) {
	cases := []struct{ in, base, level string }{
		{"claude-sonnet-4-6:high", "claude-sonnet-4-6", "high"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6", ""},
		{"gpt-5:low", "gpt-5", "low"},
		{"o3:medium", "o3", "medium"},
		{"sonnet:off", "sonnet", "off"},
		{"qwen3:8b", "qwen3:8b", ""}, // Ollama tag, not a level
		{"qwen2.5-coder:14b", "qwen2.5-coder:14b", ""},
		{":high", ":high", ""}, // no base name — leave untouched
	}
	for _, c := range cases {
		base, level := SplitThinking(c.in)
		if base != c.base || level != c.level {
			t.Errorf("SplitThinking(%q) = (%q,%q), want (%q,%q)", c.in, base, level, c.base, c.level)
		}
	}
}
