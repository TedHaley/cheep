package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMultiPathDedup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHEEP_HOME", filepath.Join(home, ".cheep"))
	work := t.TempDir()

	// Same name at three levels: the project .agents copy must win.
	write(t, filepath.Join(work, ".agents", "skills", "deploy", "SKILL.md"),
		"---\ndescription: project agents copy\n---\nbody-project")
	write(t, filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md"),
		"---\ndescription: personal claude copy\n---\nbody-personal")
	write(t, filepath.Join(home, ".cheep", "skills", "deploy.md"),
		"---\ndescription: cheep flat copy\n---\nbody-cheep")
	// And one unique per level.
	write(t, filepath.Join(work, ".claude", "skills", "review.md"), "how to review")
	write(t, filepath.Join(home, ".cheep", "skills", "legacy.md"), "old flat skill")

	got := Load(work)
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("got %d skills %v, want 3", len(got), got)
	}
	if d, ok := byName["deploy"]; !ok || d.Desc != "project agents copy" {
		t.Fatalf("dedup kept wrong copy: %+v", byName["deploy"])
	}
	if byName["deploy"].Dir == "" {
		t.Fatal("directory skill should record Dir")
	}
	if _, ok := byName["review"]; !ok {
		t.Fatal("project .claude flat skill missing")
	}
	if byName["legacy"].Dir != "" {
		t.Fatal("flat skill should have empty Dir")
	}
}

func TestSkillMdDirAndSupportingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHEEP_HOME", filepath.Join(home, ".cheep"))
	work := t.TempDir()
	dir := filepath.Join(work, ".agents", "skills", "migrate")
	write(t, filepath.Join(dir, "SKILL.md"), "---\nname: db-migrate\ndescription: run migrations\n---\nsteps here")
	write(t, filepath.Join(dir, "scripts", "run.sh"), "#!/bin/sh\n")

	tools := Tools(work)
	if tools == nil {
		t.Fatal("no tools")
	}
	var use func(map[string]any) string
	for _, tl := range tools {
		if tl.Name == "use_skill" {
			f := tl.Func
			use = func(a map[string]any) string { return f(t.Context(), a) }
		}
	}
	out := use(map[string]any{"name": "db-migrate"})
	if !strings.Contains(out, "steps here") {
		t.Fatalf("body missing: %q", out)
	}
	if !strings.Contains(out, "scripts/run.sh") {
		t.Fatalf("supporting files missing: %q", out)
	}
}

func TestLoadNoDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHEEP_HOME", filepath.Join(home, ".cheep"))
	if got := Load(t.TempDir()); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if Tools(t.TempDir()) != nil {
		t.Fatal("expected nil tools")
	}
}
