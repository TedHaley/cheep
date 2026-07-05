package prompts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListProjectShadowsGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHEEP_HOME", home)
	work := t.TempDir()

	write := func(dir, name, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, "prompts"), "review.md", "global review")
	write(filepath.Join(home, "prompts"), "release.md", "---\ndescription: cut a release\n---\nrelease steps")
	write(filepath.Join(work, ".cheep", "prompts"), "review.md", "project review of $1")

	got := List(work)
	if len(got) != 2 {
		t.Fatalf("want 2 templates, got %d: %+v", len(got), got)
	}
	if got[0].Name != "release" || got[0].Description != "cut a release" || got[0].Body != "release steps" {
		t.Errorf("release parsed wrong: %+v", got[0])
	}
	if got[1].Name != "review" || got[1].Body != "project review of $1" {
		t.Errorf("project template should shadow global: %+v", got[1])
	}
}

func TestExpand(t *testing.T) {
	cases := []struct{ body, args, want string }{
		{"fix $1 in $2", "auth login.go", "fix auth in login.go"},
		{"do: $ARGUMENTS", "all the things", "do: all the things"},
		{"missing $3 ok", "a b", "missing  ok"},
		{"no placeholders", "extra args", "no placeholders\n\nextra args"},
		{"no placeholders", "", "no placeholders"},
	}
	for _, c := range cases {
		if got := Expand(Template{Body: c.body}, c.args); got != c.want {
			t.Errorf("Expand(%q, %q) = %q, want %q", c.body, c.args, got, c.want)
		}
	}
}
