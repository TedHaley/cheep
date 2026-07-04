package core

import "strings"

// ThinkingLevels are the reasoning-effort suffixes accepted on a model name.
var ThinkingLevels = []string{"off", "low", "medium", "high"}

// SplitThinking splits an optional reasoning-level suffix off a model name:
// "claude-sonnet-4-6:high" → ("claude-sonnet-4-6", "high"). Only the exact
// levels off|low|medium|high count as suffixes, so Ollama-style tags
// ("qwen3:8b") pass through untouched.
func SplitThinking(model string) (base, level string) {
	if i := strings.LastIndex(model, ":"); i > 0 {
		s := model[i+1:]
		for _, l := range ThinkingLevels {
			if s == l {
				return model[:i], s
			}
		}
	}
	return model, ""
}
