package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

// TestMultilinePasteExpandsInput drives the model with a bracketed-paste
// KeyMsg (how bubbletea delivers a paste) and checks the input keeps the
// newlines and the box grows to show them.
func TestMultilinePasteExpandsInput(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"}},
	}
	cfg.ApplyDefaults()
	m := newModel(cfg, t.TempDir(), make(chan core.Event, 8), nil, nil, false)

	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = nm.(model)

	paste := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alpha\nbravo\ncharlie"), Paste: true}
	nm, _ = m.Update(paste)
	m = nm.(model)

	got := m.input.Value()
	if want := "alpha\nbravo\ncharlie"; got != want {
		t.Fatalf("input value = %q, want %q", got, want)
	}
	if lc := m.input.LineCount(); lc < 3 {
		t.Fatalf("LineCount = %d, want >= 3", lc)
	}
	if h := m.input.Height(); h < 3 {
		t.Fatalf("input height = %d, want >= 3 so all lines are visible", h)
	}
	view := m.View()
	for _, w := range []string{"alpha", "bravo", "charlie"} {
		if !strings.Contains(view, w) {
			t.Errorf("rendered view missing %q", w)
		}
	}
}

// TestUnbracketedPasteBurst simulates a terminal replaying a paste as raw
// keystrokes: runes and Enters arriving back-to-back must build a multi-line
// draft, not submit each line.
func TestUnbracketedPasteBurst(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"}},
	}
	cfg.ApplyDefaults()
	m := newModel(cfg, t.TempDir(), make(chan core.Event, 8), nil, nil, false)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = nm.(model)

	burst := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("alpha")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune("bravo")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune("charlie")},
	}
	for _, k := range burst { // test Updates run micro-seconds apart: a burst
		nm, _ = m.Update(k)
		m = nm.(model)
	}
	if got, want := m.input.Value(), "alpha\nbravo\ncharlie"; got != want {
		t.Fatalf("input = %q, want %q (lines must not submit)", got, want)
	}
	if h := m.input.Height(); h < 3 {
		t.Fatalf("input height = %d, want >= 3", h)
	}

	// A deliberate Enter (typed after a human-scale pause) still submits.
	time.Sleep(30 * time.Millisecond)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(model)
	if m.input.Value() != "" {
		t.Fatalf("deliberate enter should submit and clear the input, got %q", m.input.Value())
	}
}

// TestLongLineWrapGrowsInput: a single long line must grow the box by its
// wrapped rows — a 1-row box makes the textarea chase the cursor sideways.
func TestLongLineWrapGrowsInput(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"}},
	}
	cfg.ApplyDefaults()
	m := newModel(cfg, t.TempDir(), make(chan core.Event, 8), nil, nil, false)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = nm.(model)

	long := strings.Repeat("wrap this text ", 20) // ~300 chars vs ~78-wide box
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(long)})
	m = nm.(model)

	if h := m.input.Height(); h < 3 {
		t.Fatalf("input height = %d for a %d-char line in an 80-wide window; want >= 3 wrapped rows", h, len(long))
	}
}
