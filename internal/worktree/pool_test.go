//go:build unix

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newRepo creates a git repo with one commit and a .gitignore covering
// "node_modules/".
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", "-A")
	run("commit", "-m", "init")
	return dir
}

func poolFor(t *testing.T, repo string) *Pool {
	t.Helper()
	t.Setenv("CHEEP_HOME", filepath.Join(t.TempDir(), ".cheep"))
	p, err := OpenPool(repo)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPoolRecyclePreservesIgnoredArtifacts(t *testing.T) {
	repo := newRepo(t)
	p := poolFor(t, repo)

	t1, err := p.Acquire("job", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a build cache and some merged work.
	os.MkdirAll(filepath.Join(t1.Path, "node_modules", "dep"), 0o755)
	os.WriteFile(filepath.Join(t1.Path, "node_modules", "dep", "x.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(t1.Path, "b.txt"), []byte("b\n"), 0o644)
	if ok, err := t1.CommitAll("work"); err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if err := t1.MergeInto(); err != nil {
		t.Fatal(err)
	}
	p.Release(t1, true)

	t2, err := p.Acquire("job", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Path != t1.Path {
		t.Fatalf("expected slot reuse, got %s then %s", t1.Path, t2.Path)
	}
	if _, err := os.Stat(filepath.Join(t2.Path, "node_modules", "dep", "x.js")); err != nil {
		t.Fatal("gitignored artifact did not survive recycling — clean must not use -x")
	}
	if _, err := os.Stat(filepath.Join(t2.Path, "b.txt")); err != nil {
		t.Fatal("merged work missing from recycled slot")
	}
	if t2.Branch == t1.Branch {
		t.Fatal("recycled slot must get a fresh branch")
	}
}

func TestPoolQuarantinesUnlandedWork(t *testing.T) {
	repo := newRepo(t)
	p := poolFor(t, repo)

	t1, err := p.Acquire("job", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(t1.Path, "unlanded.txt"), []byte("precious\n"), 0o644)
	if _, err := t1.CommitAll("unlanded"); err != nil {
		t.Fatal(err)
	}
	p.Release(t1, false) // NOT merged

	t2, err := p.Acquire("job", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Path == t1.Path {
		t.Fatal("slot with unlanded commits was recycled — work would be destroyed")
	}
	if _, err := os.Stat(filepath.Join(t1.Path, "unlanded.txt")); err != nil {
		t.Fatal("unlanded work vanished")
	}
	var states []string
	for _, s := range p.List() {
		states = append(states, s.State)
	}
	if !strings.Contains(strings.Join(states, " "), "quarantined") {
		t.Fatalf("expected a quarantined slot, got %v", states)
	}
}

func TestPoolQuarantinesDirtyTree(t *testing.T) {
	repo := newRepo(t)
	p := poolFor(t, repo)
	t1, _ := p.Acquire("job", 1, false)
	os.WriteFile(filepath.Join(t1.Path, "a.txt"), []byte("dirty edit\n"), 0o644)
	p.Release(t1, true) // claims landed, but the tree is dirty — git is authoritative
	t2, err := p.Acquire("job", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Path == t1.Path {
		t.Fatal("dirty slot was recycled")
	}
}

func TestPoolConcurrentAcquire(t *testing.T) {
	repo := newRepo(t)
	p := poolFor(t, repo)
	var wg sync.WaitGroup
	paths := make([]string, 4)
	errs := make([]error, 4)
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, err := p.Acquire("par", i+1, false)
			errs[i] = err
			if err == nil {
				paths[i] = tr.Path
			}
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, pth := range paths {
		if errs[i] != nil {
			t.Fatalf("acquire %d: %v", i, errs[i])
		}
		if seen[pth] {
			t.Fatalf("slot %s handed out twice", pth)
		}
		seen[pth] = true
	}
}

func TestPoolCLILease(t *testing.T) {
	repo := newRepo(t)
	p := poolFor(t, repo)
	t1, err := p.Acquire("cli", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	// The CLI process exited (flock gone) but the lease must hold the slot.
	t2, err := p.Acquire("other", 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Path == t1.Path {
		t.Fatal("CLI-leased slot was handed out")
	}
	if err := p.ReleaseByPath(t1.Path, true); err != nil {
		t.Fatal(err)
	}
	t3, err := p.Acquire("after", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if t3.Path != t1.Path {
		t.Fatalf("released slot not reused: got %s want %s", t3.Path, t1.Path)
	}
}

func TestPoolFallbackTreeRemove(t *testing.T) {
	// Release on a non-pooled tree must behave like the old Remove path.
	repo := newRepo(t)
	p := poolFor(t, repo)
	tr, err := Add(repo, "eph", 1)
	if err != nil {
		t.Fatal(err)
	}
	p.Release(tr, true)
	if _, err := os.Stat(tr.Path); !os.IsNotExist(err) {
		t.Fatal("ephemeral tree not removed")
	}
}
