// Package inflight persists a marker for every delegated subtask while it
// runs, so a crash or kill never silently strands work. The marker is written
// when a delegation starts and removed when its result is integrated; markers
// still on disk at the next launch identify interrupted delegations (whose
// work, if any, survives on a quarantined worktree branch — the "no turn ends
// blind" backstop, ported from firstmate).
package inflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TedHaley/cheep/internal/config"
)

// Job is one in-flight delegated subtask.
type Job struct {
	Workdir  string    `json:"workdir"`
	Executor string    `json:"executor"`
	Kind     string    `json:"kind,omitempty"` // "" (ship) | "scout"
	Subtask  string    `json:"subtask"`
	Branch   string    `json:"branch,omitempty"` // worktree branch, when isolated
	Started  time.Time `json:"started"`
	PID      int       `json:"pid"` // owning process, so live markers are never reaped

	path string // marker file, for Clear
}

func dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "inflight"), nil
}

// Mark persists a job marker and returns the job with its path set. Failures
// are silent — supervision must never break a run.
func Mark(j Job) Job {
	d, err := dir()
	if err != nil || os.MkdirAll(d, 0o700) != nil {
		return j
	}
	if len(j.Subtask) > 500 {
		j.Subtask = j.Subtask[:500] + "…"
	}
	j.PID = os.Getpid()
	f, err := os.CreateTemp(d, fmt.Sprintf("job-%s-*.json", time.Now().UTC().Format("20060102-150405")))
	if err != nil {
		return j
	}
	j.path = f.Name()
	b, _ := json.MarshalIndent(j, "", "  ")
	_, _ = f.Write(b)
	_ = f.Close()
	return j
}

// Clear removes the job's marker (call when its result has been integrated).
func (j Job) Clear() {
	if j.path != "" {
		_ = os.Remove(j.path)
	}
}

// Stale returns markers left behind for a workdir by a previous process — the
// interrupted delegations — and removes them from disk (they are surfaced
// once; the branches they name survive independently).
func Stale(workdir string) []Job {
	d, err := dir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []Job
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(d, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var j Job
		if json.Unmarshal(b, &j) != nil || j.Workdir != workdir {
			continue
		}
		if alive(j.PID) {
			continue // another cheep is still running this delegation
		}
		j.path = p
		out = append(out, j)
		_ = os.Remove(p)
	}
	return out
}

// alive reports whether pid is a running process (never this one's).
func alive(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
