package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// unbornRepo is a git init'd dir with files but NO commit — an unborn HEAD.
func unbornRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644)
	return dir
}

func TestEnsureCommitEnablesWorktrees(t *testing.T) {
	dir := unbornRepo(t)

	// Reproduce the reported failure: worktree Add can't branch from HEAD.
	if _, err := Add(dir, "x", 1); err == nil {
		t.Fatal("expected Add to fail on an unborn HEAD")
	}

	created, err := EnsureCommit(dir)
	if err != nil || !created {
		t.Fatalf("EnsureCommit = (%v, %v), want (true, nil)", created, err)
	}

	// Now a worktree can be created, and it contains the pre-existing file.
	tr, err := Add(dir, "x", 2)
	if err != nil {
		t.Fatalf("Add after EnsureCommit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tr.Path, "README.md")); err != nil {
		t.Errorf("worktree missing the committed file: %v", err)
	}
	tr.Remove(false)

	// Idempotent once HEAD exists.
	if again, _ := EnsureCommit(dir); again {
		t.Error("EnsureCommit should be a no-op after the first commit")
	}
}

func TestEnsureCommitEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	cmd.CombinedOutput()
	if created, err := EnsureCommit(dir); err != nil || !created {
		t.Fatalf("EnsureCommit on empty repo = (%v, %v), want (true, nil)", created, err)
	}
	if !hasHEAD(dir) {
		t.Error("HEAD should exist after EnsureCommit")
	}
}
