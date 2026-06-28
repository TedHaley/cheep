// Package tui is cheep's full-screen interactive shell: a tab bar of agents
// (orchestrator + each executor the orchestrator spawns), a scrollable log for
// the focused agent, and an input box with chat/plan/auto modes.
//
// The agent core is unchanged — events (already tagged by agent name) are routed
// here from background goroutines and grouped into per-agent tabs.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/orchestrator"
)

const bannerArt = ` ██████╗██╗  ██╗███████╗███████╗██████╗ ██╗
██╔════╝██║  ██║██╔════╝██╔════╝██╔══██╗██║
██║     ███████║█████╗  █████╗  ██████╔╝██║
██║     ██╔══██║██╔══╝  ██╔══╝  ██╔═══╝ ╚═╝
╚██████╗██║  ██║███████╗███████╗██║     ██╗
 ╚═════╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝     ╚═╝`

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
	extra    []core.Tool
	onEvent  core.EventFunc
	session  *agent.Session
	buildErr error

	tabs   []*tab
	byName map[string]int
	active int
	follow bool

	vp      viewport.Model
	input   textinput.Model
	sp      spinner.Model
	w, h    int
	ready   bool
	running bool
	started time.Time
	cancel  context.CancelFunc
	queue   []string // messages typed while a task is running
	footer  string

	// overlay: "" (none) | "help" | "setup"
	overlay string
	ovTitle string
	ovInput textinput.Model
	ovVP    viewport.Model
	ovSess  *agent.Session
	ovState *configassist.State
	ovLog   []string
	ovBusy  bool
}

var (
	activeTabSt = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(lipgloss.Color("6")).Padding(0, 1)
	tabSt       = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	hintSt      = lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	barSt       = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	bulletSt    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	okSt        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	errSt       = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	userSt      = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("237"))
	todoDoneSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	todoProgSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
)

func renderTodos(args map[string]any) []string {
	ts, _ := args["todos"].([]any)
	out := []string{lipgloss.NewStyle().Bold(true).Render("Todos")}
	for _, t := range ts {
		mm, _ := t.(map[string]any)
		title, _ := mm["title"].(string)
		switch s, _ := mm["status"].(string); s {
		case "done":
			out = append(out, "  "+todoDoneSt.Render("✓ "+title))
		case "in_progress":
			out = append(out, "  "+todoProgSt.Render("◉ "+title))
		default:
			out = append(out, "  ○ "+title)
		}
	}
	return out
}

// Version is shown in the header; set by main before Run.
var Version = "dev"

// Run starts the full-screen shell.
func Run(cfg config.Config, workdir, version string, extra []core.Tool) error {
	Version = version
	events := make(chan core.Event, 1024)
	m := newModel(cfg, workdir, events, extra)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for e := range events {
			p.Send(evMsg(e))
		}
	}()
	_, err := p.Run()
	return err
}

func newModel(cfg config.Config, workdir string, events chan core.Event, extra []core.Tool) model {
	ti := textinput.New()
	ti.Placeholder = "type a task, or /help"
	ti.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	m := model{
		cfg:     cfg,
		workdir: workdir,
		mode:    orchestrator.ModeAuto,
		extra:   extra,
		follow:  true,
		onEvent: func(e core.Event) {
			select {
			case events <- e:
			default: // drop rather than block an agent if the UI falls behind
			}
		},
		tabs:   []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(cfg)}},
		byName: map[string]int{"orchestrator": 0, "cheep": 0},
		input:  ti,
		sp:     sp,
		vp:     viewport.New(80, 20),
	}
	(&m).rebuild(false)
	return m
}

// gradient palette: a smooth cyan → blue → violet → pink ramp (256-color).
var bannerRamp = []string{"51", "45", "39", "75", "111", "147", "183", "177", "213", "207"}

func gradientBanner() []string {
	lines := strings.Split(bannerArt, "\n")
	width := 0
	for _, l := range lines {
		if n := len([]rune(l)); n > width {
			width = n
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		var b strings.Builder
		for col, r := range []rune(l) {
			c := bannerRamp[col*len(bannerRamp)/(width+1)]
			b.WriteString("\x1b[38;5;" + c + "m" + string(r))
		}
		b.WriteString("\x1b[0m")
		out[i] = b.String()
	}
	return out
}

// welcomeLines renders the banner beside a bordered config box as the
// orchestrator tab's opening content (it scrolls away as you work).
func welcomeLines(cfg config.Config) []string {
	info := []string{"orchestrator | " + cfg.Orchestrator.Model}
	if len(cfg.Executors) == 0 {
		info = append(info, "mode         | solo")
	}
	for _, e := range cfg.Executors {
		info = append(info, "executor     | "+e.Model+"  ("+e.Name+")")
	}
	boxBody := strings.Join(append([]string{
		lipgloss.NewStyle().Bold(true).Render(">_ cheep " + Version),
		"",
	}, append(info, hintSt.Render(shortWorkdir()))...), "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).Render(boxBody)

	header := lipgloss.JoinHorizontal(lipgloss.Center,
		strings.Join(gradientBanner(), "\n"), "   ", box)

	out := []string{""} // top space
	out = append(out, strings.Split(header, "\n")...)
	out = append(out, "",
		hintSt.Render("Tips: shift+tab cycles modes · tab switches agents · /help"), "")
	return out
}

func shortWorkdir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(wd, home) {
		return "~" + wd[len(home):]
	}
	return wd
}

func (m *model) rebuild(keep bool) {
	var hist []core.Message
	if keep && m.session != nil {
		hist = m.session.History()
	}
	orch, err := orchestrator.Build(m.cfg, m.workdir, true, m.mode, m.extra, m.onEvent)
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
		m.vp.Height = max(3, msg.Height-5) // tab bar + rule + input + rule + hint
		m.input.Width = msg.Width - 14
		m.ovVP.Width = max(10, msg.Width-6)
		m.ovVP.Height = max(3, msg.Height-7)
		m.ovInput.Width = max(10, msg.Width-10)
		m.ready = true
		(&m).syncViewport()
		return m, nil

	case evMsg:
		(&m).applyEvent(core.Event(msg))
		return m, nil

	case doneMsg:
		m.running = false
		m.cancel = nil
		m.tabs[0].status = statusKey(msg.r.Status)
		m.footer = fmt.Sprintf("%s · %d turns · %d→%d tokens", msg.r.Status, msg.r.Turns, msg.r.InputTokens, msg.r.OutputTokens)
		(&m).syncViewport()
		if len(m.queue) > 0 { // run the next queued message
			next := m.queue[0]
			m.queue = m.queue[1:]
			return m.startTask(next)
		}
		return m, nil

	case ovDoneMsg:
		m.ovBusy = false
		return m, nil

	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		if m.overlay != "" {
			return m.updateOverlay(msg)
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			if m.running && m.cancel != nil {
				m.cancel()
				m.footer = "cancelling…"
			}
			return m, nil
		case "?":
			if m.input.Value() == "" {
				m.overlay = "help"
				return m, nil
			}
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
		if m.overlay != "" {
			m.ovVP, _ = m.ovVP.Update(msg)
			return m, nil
		}
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
	if text == "" {
		return m, nil
	}
	m.input.Reset()
	if strings.HasPrefix(text, "/") {
		return m.slash(text)
	}
	if m.session == nil {
		m.footer = "not configured: " + errText(m.buildErr)
		return m, nil
	}
	if m.running {
		// Queue it — it runs automatically when the current task finishes.
		m.queue = append(m.queue, text)
		(&m).appendLine(0, userSt.Render(" › "+text+" ")+"  "+hintSt.Render("(queued)"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	return m.startTask(text)
}

func (m model) startTask(text string) (tea.Model, tea.Cmd) {
	(&m).appendLine(0, userSt.Render(" › "+text+" "))
	m.tabs[0].status = "run"
	m.active = 0
	m.follow = true
	m.running = true
	m.started = time.Now()
	(&m).syncViewport()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	s := m.session
	run := func() tea.Msg { return doneMsg{s.SendCtx(ctx, text)} }
	return m, tea.Batch(m.sp.Tick, run)
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
		m.tabs = []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(m.cfg)}}
		m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
		m.active = 0
		(&m).rebuild(false)
		m.footer = "(cleared)"
		(&m).syncViewport()
	case "/status":
		for _, l := range statusLines(m.cfg) {
			m.appendLine(m.active, l)
		}
		m.follow = true
		(&m).syncViewport()
	case "/setup", "/config":
		return m.openSetup()
	case "/help", "/?":
		m.overlay = "help"
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
	// Setup-assistant events feed the overlay, not a tab.
	if e.Agent == "setup" {
		if line := formatEvent(e); line != "" {
			m.ovLog = append(m.ovLog, line)
			if m.overlay == "setup" {
				m.ovVP.SetContent(strings.Join(m.ovLog, "\n"))
				m.ovVP.GotoBottom()
			}
		}
		return
	}
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
			m.appendLine(idx, bulletSt.Render("●")+" "+e.Text)
		}
	case "tool_call":
		if e.Tool == "update_todos" {
			for _, l := range renderTodos(e.Args) {
				m.appendLine(idx, l)
			}
			break
		}
		m.appendLine(idx, "→ "+e.Tool+"("+shortArgs(e.Args)+")")
	case "tool_result":
		if e.Tool == "update_todos" {
			break // the checklist is shown by the tool_call; skip the raw result
		}
		g := okSt.Render("✓")
		if isErrResult(e.Result) {
			g = errSt.Render("✗")
		}
		m.appendLine(idx, "  "+g+" "+short(e.Result, 200))
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
	if m.overlay != "" {
		return m.viewOverlay()
	}
	m.input.Prompt = modePrompt(m.mode)
	var hint string
	if m.running {
		elapsed := int(time.Since(m.started).Seconds())
		q := ""
		if len(m.queue) > 0 {
			q = fmt.Sprintf(" · %d queued", len(m.queue))
		}
		hint = hintSt.Render(fmt.Sprintf("%s %s (%ds · esc to cancel%s)", m.sp.View(), verb(elapsed), elapsed, q))
	} else {
		hint = hintSt.Render("? for shortcuts")
		if m.footer != "" {
			hint = m.footer + "   " + hint
		}
	}
	rule := hintSt.Render(strings.Repeat("─", max(1, m.w)))
	return lipgloss.JoinVertical(lipgloss.Left, m.tabBar(), m.vp.View(), rule, m.input.View(), rule, hint)
}

// verb returns a rotating whimsical gerund for the working indicator.
func verb(elapsed int) string {
	verbs := []string{
		"Thinking", "Manifesting", "Conjuring", "Pondering", "Noodling", "Brewing",
		"Wrangling", "Summoning", "Percolating", "Tinkering", "Computing", "Vibing",
	}
	return verbs[(elapsed/4)%len(verbs)] + "…"
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
	sym, color := "⏵⏵", "220" // auto: yellow
	switch mode {
	case orchestrator.ModeChat:
		sym, color = "⏵", "244" // grey
	case orchestrator.ModePlan:
		sym, color = "⏸", "37" // greenish-blue
	}
	label := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(sym + " " + string(mode))
	return label + " › "
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

func isErrResult(s string) bool {
	if strings.HasPrefix(s, "ERROR") {
		return true
	}
	if i := strings.Index(s, "exit="); i >= 0 && !strings.HasPrefix(s[i+5:], "0") {
		return true
	}
	return false
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

func statusLines(c config.Config) []string {
	out := []string{"orchestrator: " + c.Orchestrator.Model}
	if len(c.Executors) == 0 {
		return append(out, "executors: (none — solo)")
	}
	for _, e := range c.Executors {
		out = append(out, "executor ("+e.Name+"): "+e.Model)
	}
	return out
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
