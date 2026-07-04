// Package dispatch loads natural-language routing rules that steer which
// executor (and optionally model) handles which kind of subtask.
//
// The split of responsibilities (borrowed from firstmate's crew-dispatch):
// the LLM does the semantic matching — rules are plain-English "when" clauses
// it weighs against the actual task, not first-match patterns — while the
// code only enforces explicitness: when rules exist, delegate() refuses tasks
// without an executor choice, so the rules can't be silently skipped.
//
// Rules live in .cheep/dispatch.json (project) or ~/.cheep/dispatch.json:
//
//	{
//	  "rules": [
//	    {"when": "trivial mechanical edits or renames",
//	     "use": {"executor": "qwen-local"},
//	     "why": "free and plenty for rote work"},
//	    {"when": "big ambiguous features or tricky debugging",
//	     "use": {"executor": "deepseek", "model": "deepseek-reasoner"},
//	     "why": "worth the stronger model"}
//	  ],
//	  "default": {"executor": "qwen-local"}
//	}
package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Use names the executor (and optionally model) a rule routes to.
type Use struct {
	Executor string `json:"executor"`
	Model    string `json:"model,omitempty"`
}

// Rule is one natural-language routing rule.
type Rule struct {
	When string `json:"when"`
	Use  Use    `json:"use"`
	Why  string `json:"why,omitempty"`
}

// Rules is a loaded dispatch file.
type Rules struct {
	Rules   []Rule `json:"rules"`
	Default *Use   `json:"default,omitempty"`
}

// Load reads the project's dispatch file, falling back to the global one.
// A missing or invalid file yields no rules (invalid files are reported).
func Load(workdir, cheepHome string) (Rules, error) {
	for _, p := range []string{
		filepath.Join(workdir, ".cheep", "dispatch.json"),
		filepath.Join(cheepHome, "dispatch.json"),
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Rules
		if err := json.Unmarshal(b, &r); err != nil {
			return Rules{}, fmt.Errorf("%s: %w", p, err)
		}
		return r, nil
	}
	return Rules{}, nil
}

// Active reports whether any routing rules are in force.
func (r Rules) Active() bool { return len(r.Rules) > 0 }

// PromptBlock renders the rules for the orchestrator's system prompt.
func (r Rules) PromptBlock() string {
	if !r.Active() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nRouting rules are in force. Weigh each subtask against ALL the \"when\" clauses " +
		"below (semantic judgment, not first-match) and set \"executor\" accordingly on every " +
		"delegated task — delegation without an explicit executor is rejected while rules exist. " +
		"Route by the rule whose rationale fits best; cost-tier guidance still applies within a rule's choice.\n")
	for _, rule := range r.Rules {
		fmt.Fprintf(&b, "- when %s → executor %q", rule.When, rule.Use.Executor)
		if rule.Use.Model != "" {
			fmt.Fprintf(&b, " (model %s)", rule.Use.Model)
		}
		if rule.Why != "" {
			b.WriteString(" — " + rule.Why)
		}
		b.WriteString("\n")
	}
	if r.Default != nil {
		fmt.Fprintf(&b, "- otherwise → executor %q\n", r.Default.Executor)
	}
	return b.String()
}
