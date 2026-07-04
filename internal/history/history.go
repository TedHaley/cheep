// Package history persists cheep conversations to ~/.cheep/history as a single
// global, time-ordered timeline. Each session is stored twice: a JSON record
// (the full message list, used to resume into the agent's context) and a
// human-readable Markdown transcript.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

// Record is one persisted conversation. Sessions form a tree: a record forked
// from an earlier point of another carries that session's ID in Parent and the
// message index the branch started from in ForkAt.
type Record struct {
	ID       string         `json:"id"`
	Parent   string         `json:"parent,omitempty"`  // session this one was forked from
	ForkAt   int            `json:"fork_at,omitempty"` // message index in the parent where the fork begins
	Started  time.Time      `json:"started"`
	Updated  time.Time      `json:"updated"`
	Workdir  string         `json:"workdir"`
	Title    string         `json:"title"`
	Messages []core.Message `json:"messages"`
}

// Meta is the lightweight summary used to list sessions.
type Meta struct {
	ID      string
	Parent  string
	Started time.Time
	Updated time.Time
	Workdir string
	Title   string
	Turns   int
}

// Dir is ~/.cheep/history.
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "history"), nil
}

// NewID returns a sortable id from a timestamp (UTC, second precision).
func NewID(t time.Time) string { return t.UTC().Format("20060102-150405") }

// UniqueID returns a NewID that doesn't collide with a session already on disk
// (forks can be created within the same second as their parent).
func UniqueID(t time.Time) string {
	d, err := Dir()
	if err != nil {
		return NewID(t)
	}
	for {
		id := NewID(t)
		if _, err := os.Stat(filepath.Join(d, id+".json")); os.IsNotExist(err) {
			return id
		}
		t = t.Add(time.Second)
	}
}

// Tree returns every session in depth-first tree order along with each row's
// depth (0 = root). Roots keep List's newest-first order; children nest under
// their parent. A parent that no longer exists makes its children roots.
func Tree() ([]Meta, []int, error) {
	metas, err := List()
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]bool{}
	for _, m := range metas {
		byID[m.ID] = true
	}
	children := map[string][]Meta{}
	var roots []Meta
	for _, m := range metas {
		if m.Parent != "" && byID[m.Parent] && m.Parent != m.ID {
			children[m.Parent] = append(children[m.Parent], m)
		} else {
			roots = append(roots, m)
		}
	}
	var outM []Meta
	var outD []int
	seen := map[string]bool{} // guards against parent cycles in corrupt data
	var walk func(m Meta, d int)
	walk = func(m Meta, d int) {
		if seen[m.ID] {
			return
		}
		seen[m.ID] = true
		outM = append(outM, m)
		outD = append(outD, d)
		for _, c := range children[m.ID] {
			walk(c, d+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return outM, outD, nil
}

// AppendRunNote appends a timestamped markdown note to ~/.cheep/history/notes.md
// — the "read in the morning" trail of what delegated batches did and how
// validation went. Failures are silent: notes must never break a run.
func AppendRunNote(workdir, md string) {
	d, err := Dir()
	if err != nil || os.MkdirAll(d, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(d, "notes.md"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	stamp := time.Now().Format("2006-01-02 15:04")
	f.WriteString("\n## " + stamp + " — " + workdir + "\n\n" + strings.TrimSpace(md) + "\n")
}

// Save writes the JSON record and the Markdown transcript.
func Save(r Record) error {
	if len(r.Messages) == 0 {
		return nil // nothing to record yet
	}
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d, r.ID+".json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, r.ID+".md"), []byte(transcript(r)), 0o600)
}

func transcript(r Record) string {
	var b strings.Builder
	b.WriteString("# cheep session " + r.ID + "\n")
	b.WriteString(r.Started.Local().Format("2006-01-02 15:04") + "  ·  " + r.Workdir + "\n\n")
	for _, m := range r.Messages {
		t := strings.TrimSpace(m.Text)
		if t == "" {
			continue
		}
		role := m.Role
		switch role {
		case "user":
			role = "User"
		case "assistant":
			role = "Assistant"
		case "tool":
			continue // tool results are noise in a transcript
		}
		b.WriteString("## " + role + "\n\n" + t + "\n\n")
	}
	return b.String()
}

// List returns all sessions, most-recently-updated first.
func List() ([]Meta, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, nil // no history yet
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		out = append(out, Meta{
			ID: r.ID, Parent: r.Parent, Started: r.Started, Updated: r.Updated,
			Workdir: r.Workdir, Title: r.Title, Turns: countTurns(r.Messages),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func countTurns(ms []core.Message) int {
	n := 0
	for _, m := range ms {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// Load reads a single session record by id.
func Load(id string) (Record, error) {
	d, err := Dir()
	if err != nil {
		return Record{}, err
	}
	b, err := os.ReadFile(filepath.Join(d, id+".json"))
	if err != nil {
		return Record{}, err
	}
	var r Record
	err = json.Unmarshal(b, &r)
	return r, err
}

// Delete removes a session's record and transcript.
func Delete(id string) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(d, id+".md"))
	return os.Remove(filepath.Join(d, id+".json"))
}
