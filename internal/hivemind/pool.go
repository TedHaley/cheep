package hivemind

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Pool is the local, deduped, signature-verified store of findings received from
// peers (and your own) — the "side context" you browse and pull from. It never
// feeds the agent automatically; the TUI/CLI reads from it on demand. Safe for
// concurrent use (the network layer writes while the UI reads).
type Pool struct {
	mu    sync.RWMutex
	byID  map[string]Finding
	muted map[string]bool // muted author ids
}

func poolPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "pool.json"), nil
}

// LoadPool reads the persisted pool (empty if none or unreadable). Findings that
// fail verification are dropped on load.
func LoadPool() *Pool {
	p := &Pool{byID: map[string]Finding{}, muted: map[string]bool{}}
	if path, err := poolPath(); err == nil {
		if b, err := os.ReadFile(path); err == nil {
			var stored struct {
				Findings []Finding `json:"findings"`
				Muted    []string  `json:"muted"`
			}
			if json.Unmarshal(b, &stored) == nil {
				for _, m := range stored.Muted {
					p.muted[m] = true
				}
				for _, f := range stored.Findings {
					if !p.muted[f.Author] && f.Verify() == nil {
						p.byID[f.ID] = f
					}
				}
			}
		}
	}
	return p
}

// Add stores a finding if its signature verifies, its author isn't muted, and it
// isn't already present. Returns true if it was newly added.
func (p *Pool) Add(f Finding) bool {
	if f.Verify() != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.muted[f.Author] {
		return false
	}
	if _, ok := p.byID[f.ID]; ok {
		return false
	}
	p.byID[f.ID] = f
	return true
}

// List returns findings newest-first, optionally filtered to those matching any
// of the given tags.
func (p *Pool) List(tags ...string) []Finding {
	want := normTags(tags)
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []Finding
	for _, f := range p.byID {
		if len(want) == 0 || anyTag(f.Tags, want) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Mute hides an author's findings, now and going forward.
func (p *Pool) Mute(author string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted[author] = true
	for id, f := range p.byID {
		if f.Author == author {
			delete(p.byID, id)
		}
	}
}

// Save persists the pool (findings + mute list) to disk.
func (p *Pool) Save() error {
	p.mu.RLock()
	stored := struct {
		Findings []Finding `json:"findings"`
		Muted    []string  `json:"muted"`
	}{}
	for _, f := range p.byID {
		stored.Findings = append(stored.Findings, f)
	}
	for m := range p.muted {
		stored.Muted = append(stored.Muted, m)
	}
	p.mu.RUnlock()

	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	path := filepath.Join(d, "pool.json")
	b, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func anyTag(have, want []string) bool {
	set := make(map[string]bool, len(have))
	for _, t := range have {
		set[t] = true
	}
	for _, w := range want {
		if set[w] {
			return true
		}
	}
	return false
}
