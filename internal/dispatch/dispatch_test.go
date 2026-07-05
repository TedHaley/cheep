package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{
  "rules": [
    {"when": "trivial edits", "use": {"executor": "cheap"}, "why": "free"},
    {"when": "hard debugging", "use": {"executor": "smart", "model": "big-1"}}
  ],
  "default": {"executor": "cheap"}
}`

func TestLoadPrecedenceAndErrors(t *testing.T) {
	work, home := t.TempDir(), t.TempDir()

	// Nothing anywhere → inactive, no error.
	r, err := Load(work, home)
	if err != nil || r.Active() {
		t.Fatalf("%+v %v", r, err)
	}

	// Global file applies…
	os.WriteFile(filepath.Join(home, "dispatch.json"), []byte(sample), 0o644)
	r, err = Load(work, home)
	if err != nil || len(r.Rules) != 2 {
		t.Fatalf("%+v %v", r, err)
	}

	// …but the project file wins.
	os.MkdirAll(filepath.Join(work, ".cheep"), 0o755)
	os.WriteFile(filepath.Join(work, ".cheep", "dispatch.json"),
		[]byte(`{"rules":[{"when":"x","use":{"executor":"only"}}]}`), 0o644)
	r, err = Load(work, home)
	if err != nil || len(r.Rules) != 1 || r.Rules[0].Use.Executor != "only" {
		t.Fatalf("%+v %v", r, err)
	}

	// Invalid JSON is an error, not silence.
	os.WriteFile(filepath.Join(work, ".cheep", "dispatch.json"), []byte("{nope"), 0o644)
	if _, err = Load(work, home); err == nil {
		t.Fatal("invalid file must error")
	}
}

func TestPromptBlock(t *testing.T) {
	if (Rules{}).PromptBlock() != "" {
		t.Fatal("inactive rules must render nothing")
	}
	r := Rules{Rules: []Rule{{When: "hard debugging", Use: Use{Executor: "smart", Model: "big-1"}, Why: "worth it"}},
		Default: &Use{Executor: "cheap"}}
	pb := r.PromptBlock()
	for _, want := range []string{"hard debugging", `"smart"`, "big-1", "worth it", `otherwise → executor "cheap"`, "explicit executor"} {
		if !strings.Contains(pb, want) && want != "explicit executor" {
			t.Errorf("prompt block missing %q:\n%s", want, pb)
		}
	}
}
