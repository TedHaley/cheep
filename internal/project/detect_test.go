package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectCommands(t *testing.T) {
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("go", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "go.mod", "module x\n")
		d := DetectCommands(dir)
		if len(d.Test) == 0 || d.Test[0] != "go test ./..." {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("node with pnpm", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "package.json", `{"scripts":{"build":"tsc","test":"vitest","lint":"eslint ."}}`)
		write(dir, "pnpm-lock.yaml", "")
		d := DetectCommands(dir)
		if len(d.Test) != 1 || d.Test[0] != "pnpm test" {
			t.Fatalf("%+v", d)
		}
		if len(d.Lint) != 1 || d.Lint[0] != "pnpm lint" {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("makefile wraps", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "go.mod", "module x\n")
		write(dir, "Makefile", "test:\n\tgo test ./...\nlint:\n\tgolangci-lint run\nVAR:=1\n")
		d := DetectCommands(dir)
		if d.Test[0] != "make test" {
			t.Fatalf("make test should lead: %+v", d.Test)
		}
		if d.Lint[0] != "make lint" {
			t.Fatalf("make lint should lead: %+v", d.Lint)
		}
	})

	t.Run("python uv", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "pyproject.toml", "[tool.ruff]\nline-length = 100\n")
		write(dir, "uv.lock", "")
		d := DetectCommands(dir)
		if d.Test[0] != "uv run pytest" {
			t.Fatalf("%+v", d)
		}
		if len(d.Lint) == 0 || d.Lint[0] != "ruff check ." {
			t.Fatalf("%+v", d)
		}
	})

	t.Run("empty", func(t *testing.T) {
		d := DetectCommands(t.TempDir())
		if len(d.Build)+len(d.Test)+len(d.Lint) != 0 {
			t.Fatalf("%+v", d)
		}
	})
}

func TestScaffoldChecksRoundTrip(t *testing.T) {
	// The scaffolded AGENTS.md's Validation section must parse back into the
	// same commands — the template and parser must never drift apart.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	wrote, err := Scaffold(dir, DetectCommands(dir), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 3 { // AGENTS.md, CLAUDE.md, toolbelt skill
		t.Fatalf("wrote %v", wrote)
	}
	c := Load(dir)
	if len(c.Checks) != 2 {
		t.Fatalf("checks did not round-trip: %+v", c.Checks)
	}
	if c.Checks[0].Script != "go test ./..." || c.Checks[1].Script != "go vet ./..." {
		t.Fatalf("%+v", c.Checks)
	}

	// CLAUDE.md must resolve to AGENTS.md.
	target, err := os.Readlink(filepath.Join(dir, "CLAUDE.md"))
	if err == nil && target != "AGENTS.md" {
		t.Fatalf("symlink target %q", target)
	}

	// Idempotent: second run writes nothing.
	wrote, err = Scaffold(dir, DetectCommands(dir), false)
	if err != nil || len(wrote) != 0 {
		t.Fatalf("not idempotent: %v %v", wrote, err)
	}

	// The toolbelt skill is discoverable by the skills loader (name check
	// via frontmatter would need the skills package; just check the file).
	b, _ := os.ReadFile(filepath.Join(dir, ".agents", "skills", "cheep-toolbelt", "SKILL.md"))
	if !strings.Contains(string(b), "cheep validate") {
		t.Fatal("toolbelt skill missing validate docs")
	}
}
