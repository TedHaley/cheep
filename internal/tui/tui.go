// Package tui is cheep's full-screen interactive shell: a tab bar of agents
// (orchestrator + each executor the orchestrator spawns), a scrollable log for
// the focused agent, and an input box with chat/plan/auto modes.
//
// The agent core is unchanged — events (already tagged by agent name) are routed
// here from background goroutines and grouped into per-agent tabs.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/orchestrator"
)

type evMsg core.Event
type doneMsg struct{ r agent.RunResult }

type tab struct {
	id     string // agent name: "orchestrator" / "qwen-local#2"
	title  string
	status string // "" | run | ok | warn | err
	lines  []string
}

type model struct {
	cfg      config.Config
	workdir  string
	mode     orchestrator.Mode
	onEvent  core.EventFunc
	session  *agent.Session
	buildErr error

	tabs   []*tab
	byName map[string]int
	active int
	follow bool

	vp      viewport.Model
	input   textinput.Model
	w, h    int
	ready   bool
	running bool
	footer  string
}

var (
	activeTabSt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(lipgloss.Color("6")).Padding(0, 1)
	tabSt       = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	hintSt      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	barSt       = lipgloss.NewStyle().Background(lipgloss.Color("236"))
)

// Run starts the full-screen shell.
func Run(cfg config.Config, workdir string) error {
	events := make(chan core.Event, 1024)
	m := newModel(cfg, workdir, events)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for e := range events {
			p.Send(evMsg(e))
		}
	}()
	_, err := p.Run()
	return err
}

func newModel(cfg config.Config, workdir string, events chan core.Event) model {
	ti := textinput.New()
	ti.Placeholder = "type a task, or /help"
	ti.Focus()

	m := model{
		cfg:     cfg,
		workdir: workdir,
		mode:    orchestrator.ModeAuto,
		follow:  true,
		onEvent: func(e core.Event) {
			select {
			case events <- e:
			default: // drop rather than block an agent if the UI falls behind
			}
		},
		tabs:   []*tab{{id: "orchestrator", title: "orchestrator"}},
		byName: map[string]int{"orchestrator": 0, "cheep": 0},
		input:  ti,
		vp:     viewport.New(80, 20),
	}
	(&m).rebuild(false)
	return m
}

func (m *model) rebuild(keep bool) {
	var hist []core.Message
	if keep && m.session != nil {
		hist = m.session.History()
	}
	orch, err := orchestrator.Build(m.cfg, m.workdir, true, m.mode, m.onEvent)
	m.buildErr = err
	if err != nil {
		m.session = nil
		return
	}
	m.session = orch.Resume(hist)
}

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.vp.Height = max(3, msg.Height-3) // tab bar (1) + footer (2)
		m.input.Width = msg.Width - 14
		m.ready = true
		(&m).syncViewport()
		return m, nil

	case evMsg:
		(&m).applyEvent(core.Event(msg))
		return m, nil

	case doneMsg:
		m.running = false
		m.tabs[0].status = statusKey(msg.r.Status)
		m.footer = fmt.Sprintf("%s · %d turns · %d→%d tokens", msg.r.Status, msg.r.Turns, msg.r.InputTokens, msg.r.OutputTokens)
		(&m).syncViewport()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "shift+tab":
			m.mode = orchestrator.NextMode(m.mode)
			(&m).rebuild(true)
			return m, nil
		case "tab":
			if len(m.tabs) > 0 {
				m.active = (m.active + 1) % len(m.tabs)
				m.follow = true
				(&m).syncViewport()
			}
			return m, nil
		case "ctrl+left":
			if len(m.tabs) > 0 {
				m.active = (m.active - 1 + len(m.tabs)) % len(m.tabs)
				(&m).syncViewport()
			}
			return m, nil
		case "ctrl+right":
			if len(m.tabs) > 0 {
				m.active = (m.active + 1) % len(m.tabs)
				(&m).syncViewport()
			}
			return m, nil
		case "pgup", "up":
			m.follow = false
			m.vp, _ = m.vp.Update(msg)
			return m, nil
		case "pgdown", "down":
			m.vp, _ = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, nil
		case "enter":
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		m.vp, _ = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if text == "" {
		return m, nil
	}
	if strings.HasPrefix(text, "/") {
		return m.slash(text)
	}
	if m.running {
		m.footer = "busy — wait for the current task to finish"
		return m, nil
	}
	if m.session == nil {
		m.footer = "not configured: " + errText(m.buildErr)
		return m, nil
	}
	(&m).appendLine(0, "› "+text)
	m.tabs[0].status = "run"
	m.active = 0
	m.follow = true
	m.running = true
	(&m).syncViewport()
	s := m.session
	return m, func() tea.Msg { return doneMsg{s.Send(text)} }
}

func (m model) slash(text string) (tea.Model, tea.Cmd) {
	switch strings.Fields(text)[0] {
	case "/exit", "/quit", "/q":
		return m, tea.Quit
	case "/chat":
		m.mode = orchestrator.ModeChat
		(&m).rebuild(true)
	case "/plan":
		m.mode = orchestrator.ModePlan
		(&m).rebuild(true)
	case "/auto":
		m.mode = orchestrator.ModeAuto
		(&m).rebuild(true)
	case "/mode":
		m.mode = orchestrator.NextMode(m.mode)
		(&m).rebuild(true)
	case "/clear":
		m.tabs = []*tab{{id: "orchestrator", title: "orchestrator"}}
		m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
		m.active = 0
		(&m).rebuild(false)
		m.footer = "(cleared)"
		(&m).syncViewport()
	case "/status":
		m.footer = statusLine(m.cfg)
	case "/config", "/setup":
		m.footer = "run `cheep " + strings.TrimPrefix(strings.Fields(text)[0], "/") + "` in a normal terminal (wizard not available in the TUI yet)"
	case "/help":
		(&m).appendLine(m.active, "commands: /chat /plan /auto · /mode · /clear · /status · /exit")
		(&m).appendLine(m.active, "keys: shift+tab=mode · tab=next agent · ctrl+←/→=agents · pgup/pgdn=scroll")
		(&m).syncViewport()
	default:
		m.footer = "unknown command " + strings.Fields(text)[0]
	}
	return m, nil
}

func (m *model) tabFor(name string) int {
	if name == "" || name == "orchestrator" || name == "cheep" {
		return 0
	}
	if i, ok := m.byName[name]; ok {
		return i
	}
	m.tabs = append(m.tabs, &tab{id: name, title: name, status: "run"})
	i := len(m.tabs) - 1
	m.byName[name] = i
	return i
}

func (m *model) appendLine(idx int, line string) {
	m.tabs[idx].lines = append(m.tabs[idx].lines, line)
}

func (m *model) applyEvent(e core.Event) {
	idx := m.tabFor(e.Agent)
	t := m.tabs[idx]
	switch e.Type {
	case "lifecycle":
		if e.Status == "start" {
			t.status = "run"
			m.appendLine(idx, "● started")
		} else {
			t.status = statusKey(e.Status)
			m.appendLine(idx, "■ "+e.Status)
		}
	case "text":
		if e.Text != "" {
			m.appendLine(idx, e.Text)
		}
	case "tool_call":
		m.appendLine(idx, "→ "+e.Tool+"("+shortArgs(e.Args)+")")
	case "tool_result":
		m.appendLine(idx, "  ← "+short(e.Result, 300))
	case "status":
		m.appendLine(idx, "• "+e.Status)
		if k := statusKey(e.Status); k != "" {
			t.status = k
		}
	case "error":
		m.appendLine(idx, "✗ "+e.Text)
		t.status = "err"
	}
	if idx == m.active {
		m.syncViewport()
	}
}

func (m *model) syncViewport() {
	if !m.ready || len(m.tabs) == 0 {
		return
	}
	m.vp.SetContent(strings.Join(m.tabs[m.active].lines, "\n"))
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m model) View() string {
	if !m.ready {
		return "starting cheep…"
	}
	m.input.Prompt = modePrompt(m.mode)
	hint := "shift+tab: mode · tab: agent · pgup/pgdn: scroll · /help"
	if m.footer != "" {
		hint = m.footer + "   " + hintSt.Render(hint)
	} else {
		hint = hintSt.Render(hint)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.tabBar(), m.vp.View(), m.input.View(), hint)
}

func (m model) tabBar() string {
	var parts []string
	for i, t := range m.tabs {
		label := glyph(t.status) + " " + t.title
		if i == m.active {
			parts = append(parts, activeTabSt.Render(label))
		} else {
			parts = append(parts, tabSt.Render(label))
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	return barSt.Width(m.w).Render(bar)
}

func modePrompt(mode orchestrator.Mode) string {
	sym := "⏵⏵"
	switch mode {
	case orchestrator.ModeChat:
		sym = "⏵"
	case orchestrator.ModePlan:
		sym = "⏸"
	}
	return fmt.Sprintf("%s %s › ", sym, mode)
}

func glyph(status string) string {
	switch status {
	case "run":
		return "●"
	case "ok":
		return "✓"
	case "warn":
		return "⚠"
	case "err":
		return "✗"
	}
	return "·"
}

func statusKey(s string) string {
	switch s {
	case "completed":
		return "ok"
	case "error", "timeout", "aborted":
		return "err"
	case "looping", "max_turns", "context_exhausted":
		return "warn"
	}
	return ""
}

func statusLine(c config.Config) string {
	if len(c.Executors) == 0 {
		return fmt.Sprintf("orchestrator %s [%s] · solo", c.Orchestrator.Model, c.Orchestrator.Provider)
	}
	names := make([]string, len(c.Executors))
	for i, e := range c.Executors {
		names[i] = e.Name
	}
	return fmt.Sprintf("orchestrator %s · executors: %s", c.Orchestrator.Model, strings.Join(names, ", "))
}

func errText(err error) string {
	if err == nil {
		return "no session"
	}
	return err.Error()
}

func shortArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		parts = append(parts, k+"="+s)
	}
	return short(strings.Join(parts, ", "), 120)
}

func short(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
