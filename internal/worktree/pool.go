package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/config"
)

// Pool reuses git worktrees across runs so gitignored build artifacts
// (node_modules, target/, .venv, Go build cache warmth) survive between
// subtasks instead of being rebuilt in a fresh temp dir each time.
//
// Safety model, fail-closed: a slot is recycled ONLY when its tree is clean
// and its HEAD is reachable from the repository's current HEAD — i.e. the
// previous work provably landed (merged) or made no commits. Anything else
// (dirty tree, unlanded commits after a crash or a failed validation) leaves
// the slot quarantined: never recycled, surfaced by List, work never lost.
//
// Concurrency: each slot is guarded by a non-blocking flock held for the
// lifetime of the acquire, so process death releases it automatically. The
// `cheep worktree` CLI can't hold a flock past its own exit, so CLI acquires
// take a lease recorded in the slot's state file instead; in-process acquires
// respect it.
type Pool struct {
	repo string // absolute path of the primary checkout
	dir  string // ~/.cheep/worktrees/<name>-<hash>
	max  int
}

// slotState is persisted next to each slot as wt-N.json.
type slotState struct {
	Branch   string    `json:"branch"`
	Base     string    `json:"base"`
	Acquired time.Time `json:"acquired_at"`
	Landed   bool      `json:"landed"`
	CLILease bool      `json:"cli_lease,omitempty"`
}

const defaultPoolSize = 8

// OpenPool returns the worktree pool for repo, creating its directory.
// Returns an error on platforms without flock support (callers fall back to
// ephemeral worktrees via Add).
func OpenPool(repo string) (*Pool, error) {
	if !poolSupported {
		return nil, fmt.Errorf("worktree pool unsupported on this platform")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(abs))
	dir := filepath.Join(home, "worktrees",
		sanitize(filepath.Base(abs))+"-"+hex.EncodeToString(sum[:4]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Pool{repo: abs, dir: dir, max: defaultPoolSize}, nil
}

// Acquire returns a pooled worktree on a fresh branch off the repo's current
// HEAD, recycling a landed slot when possible. cli marks the acquire as
// lease-based (for the `cheep worktree` CLI, which cannot hold a flock).
func (p *Pool) Acquire(name string, uniq int, cli bool) (*Tree, error) {
	head, err := git(p.repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("repo HEAD: %w", err)
	}
	head = strings.TrimSpace(head)

	branch := p.newBranch(name, uniq)
	for i := 1; i <= p.max; i++ {
		slot := filepath.Join(p.dir, fmt.Sprintf("wt-%d", i))
		lock, ok := tryLock(slot + ".lock")
		if !ok {
			continue // in use by a live process
		}
		st, _ := readState(slot + ".json")
		if st.CLILease {
			lock.Close()
			continue // leased by a CLI acquire; only `cheep worktree release` clears it
		}

		if _, err := os.Stat(slot); os.IsNotExist(err) {
			// Fresh slot.
			if _, err := git(p.repo, "worktree", "add", "-b", branch, slot, head); err != nil {
				lock.Close()
				return nil, fmt.Errorf("worktree add: %w", err)
			}
			return p.tree(slot, branch, head, lock, cli, i)
		}

		if !p.recyclable(slot, head) {
			lock.Close() // quarantined: dirty or holds unlanded work
			continue
		}
		if err := p.recycle(slot, head, branch); err != nil {
			lock.Close()
			continue // recycle failed; leave the slot alone
		}
		return p.tree(slot, branch, head, lock, cli, i)
	}
	return nil, fmt.Errorf("no free worktree slot (max %d; quarantined slots hold unlanded work — see `cheep worktree list`)", p.max)
}

// recyclable reports whether a slot provably holds no unlanded work: a clean
// tree whose HEAD is reachable from the repo's current HEAD.
func (p *Pool) recyclable(slot, repoHead string) bool {
	status, err := git(slot, "status", "--porcelain")
	if err != nil || strings.TrimSpace(status) != "" {
		return false
	}
	slotHead, err := git(slot, "rev-parse", "HEAD")
	if err != nil {
		return false
	}
	_, err = git(slot, "merge-base", "--is-ancestor", strings.TrimSpace(slotHead), repoHead)
	return err == nil
}

// recycle resets a landed slot onto a new branch at head. Crucially `clean
// -fd` has no -x: gitignored artifacts (node_modules, target/, venvs) are the
// whole reason the pool exists and must survive.
func (p *Pool) recycle(slot, head, branch string) error {
	old, _ := git(slot, "rev-parse", "--abbrev-ref", "HEAD")
	if _, err := git(slot, "checkout", "--detach", head); err != nil {
		return err
	}
	if _, err := git(slot, "reset", "--hard", head); err != nil {
		return err
	}
	if _, err := git(slot, "clean", "-fd"); err != nil {
		return err
	}
	if o := strings.TrimSpace(old); o != "" && o != "HEAD" {
		_, _ = git(p.repo, "branch", "-D", o) // landed → safe to drop
	}
	if _, err := git(slot, "switch", "-c", branch); err != nil {
		return err
	}
	return nil
}

func (p *Pool) tree(slot, branch, base string, lock *os.File, cli bool, n int) (*Tree, error) {
	writeState(slot+".json", slotState{Branch: branch, Base: base, Acquired: time.Now(), CLILease: cli})
	t := &Tree{Repo: p.repo, Path: slot, Branch: branch, Base: base, pooled: true, lock: lock}
	if cli {
		// The CLI process exits after printing the path; the lease in the
		// state file is the lock. Release the flock now.
		lock.Close()
		t.lock = nil
	}
	_ = n
	return t, nil
}

// Release returns a pooled tree to the pool. landed must be true only when
// the work is merged (or there was nothing to keep); it makes the slot
// recyclable and drops the branch. With landed=false the slot is left
// quarantined with branch and commits intact.
func (p *Pool) Release(t *Tree, landed bool) {
	if !t.pooled {
		t.Remove(!landed)
		return
	}
	st, _ := readState(t.Path + ".json")
	st.Landed, st.CLILease = landed, false
	if landed {
		// Park detached on the base so the branch can be deleted; the next
		// Acquire re-verifies with git before trusting any of this.
		_, _ = git(t.Path, "checkout", "--detach", t.Base)
		_, _ = git(t.Repo, "branch", "-D", t.Branch)
	}
	writeState(t.Path+".json", st)
	if t.lock != nil {
		t.lock.Close()
		t.lock = nil
	}
}

// SlotInfo describes one pool slot for `cheep worktree list`.
type SlotInfo struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	State  string `json:"state"` // free | in-use | leased | quarantined
}

// List reports every slot's state.
func (p *Pool) List() []SlotInfo {
	head, _ := git(p.repo, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	var out []SlotInfo
	for i := 1; i <= p.max; i++ {
		slot := filepath.Join(p.dir, fmt.Sprintf("wt-%d", i))
		if _, err := os.Stat(slot); os.IsNotExist(err) {
			continue
		}
		st, _ := readState(slot + ".json")
		info := SlotInfo{Path: slot, Branch: st.Branch}
		lock, ok := tryLock(slot + ".lock")
		switch {
		case !ok:
			info.State = "in-use"
		case st.CLILease:
			info.State = "leased"
		case p.recyclable(slot, head):
			info.State = "free"
		default:
			info.State = "quarantined"
		}
		if lock != nil {
			lock.Close()
		}
		out = append(out, info)
	}
	return out
}

// ReleaseByPath releases a CLI-leased (or quarantined) slot by path — the
// `cheep worktree release` entry point. force clears a quarantined slot's
// lease even though its work may be unlanded (the branch still survives).
func (p *Pool) ReleaseByPath(path string, landed bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Dir(abs) != p.dir {
		return fmt.Errorf("%s is not a slot of this repo's pool", path)
	}
	st, err := readState(abs + ".json")
	if err != nil {
		return fmt.Errorf("no state for %s: %w", path, err)
	}
	t := &Tree{Repo: p.repo, Path: abs, Branch: st.Branch, Base: st.Base, pooled: true}
	p.Release(t, landed)
	return nil
}

func (p *Pool) newBranch(name string, uniq int) string {
	base := "cheep/" + sanitize(name)
	branch := fmt.Sprintf("%s-%d", base, uniq)
	for branchExists(p.repo, branch) {
		uniq++
		branch = fmt.Sprintf("%s-%d", base, uniq)
	}
	return branch
}

func readState(path string) (slotState, error) {
	var st slotState
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func writeState(path string, st slotState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(path, b, 0o600)
}
