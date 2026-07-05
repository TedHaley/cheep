package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

// TestArrowKeysMoveCursor: left arrow must move the caret so an inserted char
// lands mid-string — regression for CursorEnd firing on every keystroke.
func TestArrowKeysMoveCursor(t *testing.T) {
	t.Setenv("CHEEP_HOME", t.TempDir())
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"}},
	}
	cfg.ApplyDefaults()
	m := newModel(cfg, t.TempDir(), make(chan core.Event, 8), nil, nil, false)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = nm.(model)

	send := func(k tea.KeyMsg) {
		nm, _ := m.Update(k)
		m = nm.(model)
	}
	for _, r := range "abc" {
		send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	send(tea.KeyMsg{Type: tea.KeyLeft})
	send(tea.KeyMsg{Type: tea.KeyLeft})
	send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})

	if got, want := m.input.Value(), "aXbc"; got != want {
		t.Fatalf("input = %q, want %q — left arrow isn't moving the cursor", got, want)
	}
}
