// Package worktree gives each parallel executor an isolated git worktree so
// concurrent file edits cannot clobber one another.
//
// Each delegated subtask runs on its own branch in its own working directory.
// When the executor finishes, its changes are committed on that branch and
// merged back into the base branch. Independent work merges cleanly; genuine
// conflicts are reported and left on the branch for resolution.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsRepo reports whether dir is inside a git working tree.
func IsRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// Tree is one isolated worktree on its own branch.
type Tree struct {
	Repo   string // the original repository working dir
	Path   string // the isolated worktree directory
	Branch string // the branch checked out in the worktree
	Base   string // the commit the branch started from

	pooled bool     // owned by a Pool: return via Release, not Remove
	lock   *os.File // slot flock, held while acquired (pooled only)
}

// Add creates a worktree on a fresh branch off the repo's current HEAD.
// uniq must be unique within a run (e.g. an incrementing counter).
func Add(repo, name string, uniq int) (*Tree, error) {
	_, _ = git(repo, "worktree", "prune") // clear stale worktree entries
	base := "cheep/" + sanitize(name)
	branch := fmt.Sprintf("%s-%d", base, uniq)
	for branchExists(repo, branch) { // avoid colliding with leftover branches
		uniq++
		branch = fmt.Sprintf("%s-%d", base, uniq)
	}
	path, err := os.MkdirTemp("", "cheep-wt-")
	if err != nil {
		return nil, err
	}
	if _, err := git(repo, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
		os.RemoveAll(path)
		return nil, fmt.Errorf("worktree add: %w", err)
	}
	head, _ := git(path, "rev-parse", "HEAD")
	return &Tree{Repo: repo, Path: path, Branch: branch, Base: strings.TrimSpace(head)}, nil
}

func branchExists(repo, branch string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// CommitAll stages and commits everything in the worktree. Returns false (and no
// error) when there was nothing to commit.
func (t *Tree) CommitAll(msg string) (committed bool, err error) {
	if _, err := git(t.Path, "add", "-A"); err != nil {
		return false, err
	}
	status, err := git(t.Path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := git(t.Path, "-c", "user.name=cheep", "-c", "user.email=cheep@localhost",
		"commit", "-m", msg); err != nil {
		return false, err
	}
	return true, nil
}

// MergeInto merges the worktree's branch into whatever is checked out in the
// repo. On conflict it aborts the merge and returns an error; the branch is
// left intact so the work is not lost.
func (t *Tree) MergeInto() error {
	if _, err := git(t.Repo, "merge", "--no-ff", "-m",
		"cheep: merge "+t.Branch, t.Branch); err != nil {
		_, _ = git(t.Repo, "merge", "--abort")
		return fmt.Errorf("merge conflict on %s", t.Branch)
	}
	return nil
}

// Remove deletes the worktree directory and its registration. If keepBranch is
// false the branch is deleted too (use false after a successful merge).
func (t *Tree) Remove(keepBranch bool) {
	_, _ = git(t.Repo, "worktree", "remove", "--force", t.Path)
	os.RemoveAll(t.Path)
	if !keepBranch {
		_, _ = git(t.Repo, "branch", "-D", t.Branch)
	}
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	name = strings.Trim(name, "-")
	if name == "" {
		return "exec"
	}
	return filepath.Base(name)
}
