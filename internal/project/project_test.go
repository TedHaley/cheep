package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChecks(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want []Check
	}{
		{"no section", "# Hi\n```check\ngo test\n```\n", nil},
		{"fenced named", "## Validation\n```check name=test\ngo test ./...\n```\n",
			[]Check{{"test", "go test ./..."}}},
		{"fenced bash", "# Validation\n```bash\nnpm test\n```\n",
			[]Check{{"check-1", "npm test"}}},
		{"fenced plain", "### validation\n```\nmake lint\n```\n",
			[]Check{{"check-1", "make lint"}}},
		{"tilde fence", "## Validation\n~~~check\ngo vet ./...\n~~~\n",
			[]Check{{"check-1", "go vet ./..."}}},
		{"non-check lang ignored", "## Validation\n```json\n{\"a\":1}\n```\n", nil},
		{"bullets", "## Validation\n- `go test ./...`\n* `go vet ./...`\n",
			[]Check{{"check-1", "go test ./..."}, {"check-2", "go vet ./..."}}},
		{"section ends at equal heading", "## Validation\n```check\na\n```\n## Other\n```check\nb\n```\n",
			[]Check{{"check-1", "a"}}},
		{"deeper heading stays inside", "## Validation\n### unit\n```check\na\n```\n",
			[]Check{{"check-1", "a"}}},
		{"multi-line script", "## Validation\n```check name=e2e\nset -e\nmake build\nmake e2e\n```\n",
			[]Check{{"e2e", "set -e\nmake build\nmake e2e"}}},
		{"unterminated fence", "## Validation\n```check\ngo test\n",
			[]Check{{"check-1", "go test"}}},
		{"empty block skipped", "## Validation\n```check\n\n```\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseChecks(c.md)
			if len(got) != len(c.want) {
				t.Fatalf("got %d checks %v, want %d", len(got), got, len(c.want))
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("check %d: got %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestLoadPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("global rules"), 0o644)

	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	sub := filepath.Join(repo, "pkg", "deep")
	os.MkdirAll(sub, 0o755)

	// CLAUDE.md is the fallback…
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("claude rules"), 0o644)
	c := Load(sub)
	if c.Local != "claude rules" || filepath.Base(c.LocalPath) != "CLAUDE.md" {
		t.Fatalf("CLAUDE.md fallback: got %q from %q", c.Local, c.LocalPath)
	}
	if c.Global != "global rules" {
		t.Fatalf("global: got %q", c.Global)
	}

	// …but AGENTS.md wins.
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("agents rules"), 0o644)
	c = Load(sub)
	if c.Local != "agents rules" {
		t.Fatalf("AGENTS.md precedence: got %q", c.Local)
	}
	if c.Root != repo {
		t.Fatalf("root: got %q want %q", c.Root, repo)
	}

	// A block is produced, labeled, and ordered global-then-local.
	pb := c.PromptBlock()
	gi, li := strings.Index(pb, "global rules"), strings.Index(pb, "agents rules")
	if gi < 0 || li < 0 || gi > li {
		t.Fatalf("prompt block ordering wrong:\n%s", pb)
	}
}

func TestLoadStopsAtRepoRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	outer := t.TempDir()
	os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("outer"), 0o644)
	repo := filepath.Join(outer, "repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	if c := Load(repo); c.Local != "" {
		t.Fatalf("walked past repo root: got %q", c.Local)
	}
}

func TestLoadMissingEverything(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := Load(t.TempDir())
	if c.PromptBlock() != "" {
		t.Fatalf("expected empty block, got %q", c.PromptBlock())
	}
}

func TestAppendLesson(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	// No file at all: creates AGENTS.md with the section.
	if _, err := AppendLesson(dir, "first lesson"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(b), "## Lessons\n- first lesson") {
		t.Fatalf("created file wrong:\n%s", b)
	}

	// Existing section with a following heading: inserts inside the section.
	body := "# P\n\n## Lessons\n- old\n\n## After\ntext\n"
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(body), 0o644)
	if _, err := AppendLesson(dir, "second"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	s := string(b)
	if !strings.Contains(s, "- old\n- second") {
		t.Fatalf("insertion point wrong:\n%s", s)
	}
	if strings.Index(s, "- second") > strings.Index(s, "## After") {
		t.Fatalf("lesson landed after next section:\n%s", s)
	}

	// File exists but no Lessons section: appends one.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# P\nstuff\n"), 0o644)
	if _, err := AppendLesson(dir, "third"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(b), "## Lessons\n- third") {
		t.Fatalf("section not appended:\n%s", b)
	}
}

func TestAppendLessonNeverWritesClaudeMd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# C\n"), 0o644)
	path, err := AppendLesson(dir, "goes to agents")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "AGENTS.md" {
		t.Fatalf("wrote to %s", path)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); strings.Contains(string(b), "goes to agents") {
		t.Fatal("CLAUDE.md was modified")
	}
}
