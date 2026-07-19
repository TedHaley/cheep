package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/capabilities"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

func testModel(t *testing.T) model {
	t.Helper()
	t.Setenv("CHEEP_HOME", t.TempDir())
	cfg := config.Config{
		Orchestrator: config.Agent{Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"},
		Executors:    []config.Agent{{Name: "e1", Provider: "openai", Endpoint: "http://127.0.0.1:1", Model: "m"}},
	}
	cfg.ApplyDefaults()
	m := newModel(cfg, t.TempDir(), make(chan core.Event, 8), nil, nil, false)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return nm.(model)
}

// TestExecutorTabHidesInput: on an executor tab the input box is gone and a
// read-only notice takes its place; keystrokes don't reach the input.
func TestExecutorTabHidesInput(t *testing.T) {
	m := testModel(t)
	(&m).tabFor("e1") // spawn the executor tab
	m.active = 1
	(&m).syncViewport()

	parts := m.belowViewport()
	joined := stripAnsi(strings.Join(parts, "\n"))
	if !strings.Contains(joined, "read-only") {
		t.Fatalf("executor tab should show a read-only notice, got %q", joined)
	}
	if strings.Contains(joined, "type a task") {
		t.Fatalf("executor tab should not render the input placeholder: %q", joined)
	}

	// Typing on an executor tab must not modify the input draft.
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = nm.(model)
	if m.input.Value() != "" {
		t.Fatalf("typing on an executor tab leaked into the input: %q", m.input.Value())
	}

	// On the orchestrator tab the input box is present again.
	m.active = 0
	joined = stripAnsi(strings.Join(m.belowViewport(), "\n"))
	if !strings.Contains(joined, "type a task") {
		t.Fatalf("orchestrator tab should render the input box, got %q", joined)
	}
}

// TestCtrlCClearsDraft: with text in the box, Ctrl+C clears the draft instead
// of quitting; history browsing is reset too.
func TestCtrlCClearsDraft(t *testing.T) {
	m := testModel(t)
	m.inputHist = []string{"old message"}
	m.histIdx = 0 // pretend we're browsing history
	m.input.SetValue("a half-typed thought")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = nm.(model)
	if m.input.Value() != "" {
		t.Fatalf("Ctrl+C should clear the draft, got %q", m.input.Value())
	}
	if cmd != nil {
		t.Fatalf("Ctrl+C on a non-empty draft should not quit")
	}
	if m.histIdx != len(m.inputHist) {
		t.Fatalf("Ctrl+C should reset history browsing, histIdx = %d", m.histIdx)
	}
}

// TestNewlineKeepsEarlierLinesVisible: inserting a line break must not scroll
// the earlier lines out of the input box (the bug where the previous line
// vanished until you pressed ↑). We assert the first line is still rendered.
func TestNewlineKeepsEarlierLinesVisible(t *testing.T) {
	m := testModel(t)
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m = nm.(model)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true}) // shift+enter -> alt+enter
	m = nm.(model)

	if m.input.Value() != "hello\n" {
		t.Fatalf("newline not inserted, value = %q", m.input.Value())
	}
	if view := stripAnsi(m.input.View()); !strings.Contains(view, "hello") {
		t.Fatalf("first line scrolled out of view after newline; box shows %q", view)
	}
}

// TestEscStopsShellCommand: a running !<command> is interruptible — submitting
// one arms a cancel func, and Esc fires it.
func TestEscStopsShellCommand(t *testing.T) {
	m := testModel(t)
	m.input.SetValue("!sleep 30")
	nm, cmd := m.submit()
	m = nm.(model)
	if !m.running || m.cancel == nil {
		t.Fatalf("shell command should set running + cancel (running=%v hasCancel=%v)", m.running, m.cancel != nil)
	}
	if cmd == nil {
		t.Fatalf("expected a command to run the shell process")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm.(model)
	if m.footer != "cancelling…" {
		t.Fatalf("Esc should cancel the running command, footer = %q", m.footer)
	}
}

// TestSuggestionGhostTextAndTabAccept: a next-step suggestion shows as ghost
// text in the input placeholder (not numbered chips), and Tab fills it in.
func TestSuggestionGhostTextAndTabAccept(t *testing.T) {
	m := testModel(t)
	nm, _ := m.Update(suggestionsMsg{sug: []string{"run the tests", "open a PR"}})
	m = nm.(model)
	if m.input.Placeholder != "run the tests" {
		t.Fatalf("top suggestion should be ghost text, placeholder = %q", m.input.Placeholder)
	}

	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(model)
	if m.input.Value() != "run the tests" {
		t.Fatalf("Tab should fill the suggestion, value = %q", m.input.Value())
	}
	if len(m.suggestions) != 0 {
		t.Fatalf("suggestions should clear after accept, got %v", m.suggestions)
	}
	if m.input.Placeholder != defaultPlaceholder {
		t.Fatalf("placeholder should reset to the default hint, got %q", m.input.Placeholder)
	}
}

// TestTabSwitchesAgentsWithoutSuggestion: with no suggestion showing, Tab keeps
// its old job of moving to the next agent tab.
func TestTabSwitchesAgentsWithoutSuggestion(t *testing.T) {
	m := testModel(t)
	(&m).tabFor("e1") // now two tabs: orchestrator + executor
	if m.active != 0 {
		t.Fatalf("precondition: expected to start on the orchestrator tab")
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = nm.(model)
	if m.active != 1 {
		t.Fatalf("Tab with no suggestion should switch agents, active = %d", m.active)
	}
}

// TestWelcomeLinesFitWidth: no banner line may exceed the terminal width — an
// over-wide line soft-wraps in the scrollback and garbles on resize/scroll.
func TestWelcomeLinesFitWidth(t *testing.T) {
	cfg := config.Config{
		Orchestrator: config.Agent{Model: "provider/a-deliberately-long-model-name-3.6-35b-a3b"},
		Executors:    []config.Agent{{Name: "e1", Model: "provider/a-deliberately-long-model-name-3.6-35b-a3b"}},
	}
	cfg.ApplyDefaults()
	for _, w := range []int{40, 60, 80, 100, 140} {
		for _, l := range welcomeLines(cfg, nil, w) {
			if got := lipgloss.Width(l); got > w {
				t.Errorf("width %d: banner line is %d cols wide: %q", w, got, l)
			}
		}
	}
}

// TestUserLineBlock: user messages render as a uniform-width grey block (the
// color itself is stripped by lipgloss in a non-color test env, so we assert
// the width-padding that makes the tint read as one block, plus that the style
// actually carries a background).
func TestUserLineBlock(t *testing.T) {
	if userMsgSt.GetBackground() == (lipgloss.NoColor{}) {
		t.Fatal("user message style should have a background color set")
	}
	rendered := userLine("hello there", 80)
	if !strings.Contains(rendered, "hello there") {
		t.Fatalf("user line should contain the text, got %q", rendered)
	}
	for _, l := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(l); got != 78 {
			t.Fatalf("each row should pad to a uniform 78-col block, got %d: %q", got, l)
		}
	}
}

// TestAutoImproveToggleAndInstall: /autoimprove toggles the setting, and
// installing a pending capability adds its MCP to the config.
func TestAutoImproveToggleAndInstall(t *testing.T) {
	m := testModel(t)
	if m.cfg.AutoImproveOff {
		t.Fatal("auto-improve should default ON")
	}
	nm, _ := m.slash("/autoimprove off")
	m = nm.(model)
	if !m.cfg.AutoImproveOff {
		t.Fatal("/autoimprove off should disable it")
	}

	// A pending suggestion, installed via /autoimprove install, lands in config.MCP.
	m.pendingCap = capabilities.Catalog[0]
	nm, _ = m.slash("/autoimprove install")
	m = nm.(model)
	if !capabilities.Installed(m.cfg, capabilities.Catalog[0].Name) {
		t.Fatalf("install should add %q to config.MCP", capabilities.Catalog[0].Name)
	}
	if m.pendingCap.Name != "" {
		t.Fatal("pending capability should clear after install")
	}
}

// TestQueueRemoval: /queue rm N drops one message; /queue clear empties it.
func TestQueueRemoval(t *testing.T) {
	m := testModel(t)
	m.queue = []string{"first", "second", "third"}

	nm, _ := m.slash("/queue rm 2")
	m = nm.(model)
	if got := strings.Join(m.queue, ","); got != "first,third" {
		t.Fatalf("after /queue rm 2, queue = %q, want first,third", got)
	}

	// /dequeue N and a bare /queue N also remove by index.
	nm, _ = m.slash("/dequeue 1")
	m = nm.(model)
	if got := strings.Join(m.queue, ","); got != "third" {
		t.Fatalf("after /dequeue 1, queue = %q, want third", got)
	}

	// out-of-range is rejected, queue unchanged
	nm, _ = m.slash("/queue rm 9")
	m = nm.(model)
	if got := strings.Join(m.queue, ","); got != "third" {
		t.Fatalf("out-of-range remove changed the queue: %q", got)
	}

	nm, _ = m.slash("/queue clear")
	m = nm.(model)
	if len(m.queue) != 0 {
		t.Fatalf("after /queue clear, queue still has %d items", len(m.queue))
	}
}
