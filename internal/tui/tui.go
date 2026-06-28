// Package tui is cheep's full-screen interactive shell: a tab bar of agents
// (orchestrator + each executor the orchestrator spawns), a scrollable log for
// the focused agent, and an input box with chat/plan/auto modes.
//
// The agent core is unchanged — events (already tagged by agent name) are routed
// here from background goroutines and grouped into per-agent tabs.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
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
	todos  []todoItem // latest checklist (rendered as a sticky header, updated in place)
}

type todoItem struct{ title, status string }

type model struct {
	cfg       config.Config
	workdir   string
	mode      orchestrator.Mode
	extraOrch []core.Tool
	extraExec []core.Tool
	onEvent   core.EventFunc
	session   *agent.Session
	buildErr  error

	tabs   []*tab
	byName map[string]int
	active int
	follow bool

	vp      viewport.Model
	input   textarea.Model
	sp      spinner.Model
	w, h    int
	ready   bool
	running bool
	started time.Time
	cancel  context.CancelFunc
	queue   []string // messages typed while a task is running
	footer  string

	openTodos int  // non-done todos from the orchestrator's latest update_todos
	delegated bool // did the orchestrator call delegate this run?
	nudges    int  // auto-continue count since the last user message
	keepTabs  bool // keep finished executor tabs (else auto-close at turn end)

	usage      map[string][2]int // model -> {input, output} tokens this session
	usageOrder []string          // models in first-seen order

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
	userSt      = lipgloss.NewStyle().Bold(true)
	todoDoneSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	todoProgSt  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
)

// renderMarkdown renders assistant text as styled markdown, bulleted on line 1.
func renderMarkdown(md string, width int) []string {
	if width < 20 {
		width = 80
	}
	bullet := bulletSt.Render("●") + " "
	// Use a fixed style, NOT WithAutoStyle — auto-style queries the terminal
	// (OSC 11) at runtime, which corrupts Bubble Tea's stdin/stdout.
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width-2))
	if err != nil {
		return []string{bullet + md}
	}
	out, err := r.Render(md)
	if err != nil {
		return []string{bullet + md}
	}
	lines := strings.Split(strings.Trim(out, "\n"), "\n")
	if len(lines) > 0 {
		lines[0] = bullet + strings.TrimLeft(lines[0], " ")
	}
	return lines
}

// delegateLines turns a delegate tool call into one readable line per subtask.
func delegateLines(args map[string]any, cfg config.Config) []string {
	tasks, _ := args["tasks"].([]any)
	var out []string
	for _, t := range tasks {
		mm, _ := t.(map[string]any)
		ex, _ := mm["executor"].(string)
		sub, _ := mm["subtask"].(string)
		target := ex
		for _, e := range cfg.Executors {
			if e.Name == ex {
				target = ex + " (" + e.Model + ")"
				break
			}
		}
		out = append(out, todoProgSt.Render("→ delegate")+" "+short(sub, 64)+hintSt.Render(" → "+target))
	}
	if len(out) == 0 {
		out = []string{todoProgSt.Render("→ delegate") + " (no tasks)"}
	}
	return out
}

// delegateResultLines collapses the JSON result into one line per executor.
func delegateResultLines(result string) []string {
	var rs []struct{ Executor, Status string }
	if json.Unmarshal([]byte(result), &rs) != nil || len(rs) == 0 {
		return nil
	}
	var out []string
	for _, r := range rs {
		g := okSt.Render("✓")
		if r.Status != "completed" {
			g = errSt.Render("✗")
		}
		out = append(out, "  "+g+" "+r.Executor+hintSt.Render(" · "+r.Status))
	}
	return out
}

func parseTodos(args map[string]any) []todoItem {
	ts, _ := args["todos"].([]any)
	out := make([]todoItem, 0, len(ts))
	for _, t := range ts {
		mm, _ := t.(map[string]any)
		title, _ := mm["title"].(string)
		status, _ := mm["status"].(string)
		out = append(out, todoItem{title: title, status: status})
	}
	return out
}

// todoHeaderLines renders the checklist as a sticky header (nil if no todos).
func todoHeaderLines(todos []todoItem) []string {
	if len(todos) == 0 {
		return nil
	}
	out := []string{lipgloss.NewStyle().Bold(true).Render("Todos")}
	for _, t := range todos {
		switch t.status {
		case "done":
			out = append(out, "  "+todoDoneSt.Render("✓ "+t.title))
		case "in_progress":
			out = append(out, "  "+todoProgSt.Render("◉ "+t.title))
		default:
			out = append(out, "  ○ "+t.title)
		}
	}
	return out
}

func countOpenItems(todos []todoItem) int {
	n := 0
	for _, t := range todos {
		if t.status != "done" {
			n++
		}
	}
	return n
}

// Version is shown in the header; set by main before Run.
var Version = "dev"

// Run starts the full-screen shell.
func Run(cfg config.Config, workdir, version string, extraOrch, extraExec []core.Tool) error {
	Version = version
	events := make(chan core.Event, 1024)
	m := newModel(cfg, workdir, events, extraOrch, extraExec)
	// Mouse on for wheel/trackpad scrolling. (To select/copy text, hold Option on
	// macOS or Shift elsewhere to bypass mouse tracking.)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for e := range events {
			p.Send(evMsg(e))
		}
	}()
	_, err := p.Run()
	return err
}

func newModel(cfg config.Config, workdir string, events chan core.Event, extraOrch, extraExec []core.Tool) model {
	ta := textarea.New()
	ta.Placeholder = "type a task, or /help"
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.SetHeight(1)
	ta.CharLimit = 0
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle() // no current-line highlight
	ta.Focus()
	// Enter submits (handled in Update); these insert a newline instead.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	m := model{
		cfg:       cfg,
		workdir:   workdir,
		mode:      orchestrator.ModeAuto,
		extraOrch: extraOrch,
		extraExec: extraExec,
		keepTabs:  cfg.KeepTabs,
		follow:    true,
		onEvent: func(e core.Event) {
			select {
			case events <- e:
			default: // drop rather than block an agent if the UI falls behind
			}
		},
		tabs:   []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(cfg)}},
		byName: map[string]int{"orchestrator": 0, "cheep": 0},
		usage:  map[string][2]int{},
		input:  ta,
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
	// Collapse interchangeable executors (same model) into one line; the model
	// picks which instance to use, so the type is what matters.
	counts := map[string]int{}
	var order []string
	for _, e := range cfg.Executors {
		if counts[e.Model] == 0 {
			order = append(order, e.Model)
		}
		counts[e.Model]++
	}
	for _, model := range order {
		info = append(info, "executor     | "+model)
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
		hintSt.Render("Tips: shift+tab cycles modes · multi-agent delegation runs in AUTO mode · /help"), "")
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
	orch, err := orchestrator.Build(m.cfg, m.workdir, true, m.mode, m.extraOrch, m.extraExec, m.onEvent)
	m.buildErr = err
	if err != nil {
		m.session = nil
		return
	}
	m.session = orch.Resume(hist)
}

func (m model) Init() tea.Cmd { return textarea.Blink }

// relayout sizes the input to its content (1–6 lines) and gives the rest to the log.
func (m *model) relayout() {
	lines := strings.Count(m.input.Value(), "\n") + 1
	if lines < 1 {
		lines = 1
	}
	if lines > 6 {
		lines = 6
	}
	m.input.SetHeight(lines)
	header := 0
	if len(m.tabs) > 0 {
		header = len(todoHeaderLines(m.tabs[m.active].todos))
	}
	m.vp.Height = max(3, m.h-4-lines-header) // tab bar + rule + rule + hint = 4
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.input.SetWidth(msg.Width - 2)
		m.ovVP.Width = max(10, msg.Width-6)
		m.ovVP.Height = max(3, msg.Height-7)
		m.ovInput.Width = max(10, msg.Width-10)
		m.ready = true
		(&m).relayout()
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
		if m.keepTabs {
			if len(m.tabs) > 1 {
				m.footer += "   tab → executor, ctrl+w to close"
			}
		} else {
			(&m).closeFinishedTabs()
		}
		(&m).syncViewport()
		// Backstop: orchestrator stopped with unfinished todos and never delegated.
		if m.mode == orchestrator.ModeAuto && len(m.cfg.Executors) > 0 &&
			msg.r.Status == "completed" && m.openTodos > 0 && !m.delegated && m.nudges < 2 {
			m.nudges++
			nudge := "You stopped, but there are unfinished todos and you did not call the " +
				"delegate tool. Use delegate now to run the open items in parallel across the " +
				"executors, then verify the results. Act — do not just describe."
			return m.runMessage(nudge, hintSt.Render("↻ auto-continue: finishing open todos"))
		}
		if len(m.queue) > 0 { // run the next queued message
			next := m.queue[0]
			m.queue = m.queue[1:]
			m.nudges, m.openTodos = 0, 0
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
		case "ctrl+w":
			if m.active > 0 {
				(&m).closeTab(m.active)
			} else {
				m.footer = "switch to an executor tab (tab) to close it"
			}
			return m, nil
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
		case "pgup":
			m.follow = false
			m.vp, _ = m.vp.Update(msg)
			return m, nil
		case "pgdown":
			m.vp, _ = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, nil
		case "enter":
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		(&m).relayout()
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
	(&m).relayout()
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
		(&m).appendLine(0, userSt.Render("› "+text)+"  "+hintSt.Render("(queued)"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	m.nudges = 0
	m.openTodos = 0
	return m.startTask(text)
}

func (m model) startTask(text string) (tea.Model, tea.Cmd) {
	return m.runMessage(text, userSt.Render("› "+text))
}

// runMessage sends text to the orchestrator, showing `display` in the log.
func (m model) runMessage(text, display string) (tea.Model, tea.Cmd) {
	(&m).appendLine(0, display)
	m.tabs[0].status = "run"
	m.active = 0
	m.follow = true
	m.running = true
	m.delegated = false
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
	case "/close":
		if m.active == 0 {
			m.footer = "can't close the orchestrator tab"
		} else {
			(&m).closeTab(m.active)
		}
	case "/keeptabs":
		m.keepTabs = !m.keepTabs
		m.cfg.KeepTabs = m.keepTabs
		_ = config.Save(m.cfg)
		if m.keepTabs {
			m.footer = "keep-tabs ON — finished executor tabs stay until closed"
		} else {
			m.footer = "keep-tabs OFF — finished executor tabs auto-close at turn end"
		}
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
	case "/tokens":
		for _, l := range m.tokenLines() {
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

// closeFinishedTabs removes executor tabs that are no longer running.
func (m *model) closeFinishedTabs() {
	kept := []*tab{m.tabs[0]}
	for _, t := range m.tabs[1:] {
		if t.status == "run" || t.status == "" {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(m.tabs) {
		return
	}
	m.tabs = kept
	m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
	for i, t := range m.tabs {
		m.byName[t.id] = i
	}
	if m.active >= len(m.tabs) {
		m.active = len(m.tabs) - 1
	}
	m.syncViewport()
}

// closeTab removes an executor tab (never the orchestrator at index 0).
func (m *model) closeTab(idx int) {
	if idx <= 0 || idx >= len(m.tabs) {
		return
	}
	m.tabs = append(m.tabs[:idx], m.tabs[idx+1:]...)
	m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
	for i, t := range m.tabs {
		m.byName[t.id] = i
	}
	if m.active >= len(m.tabs) {
		m.active = len(m.tabs) - 1
	}
	m.follow = true
	m.syncViewport()
}

func (m *model) applyEvent(e core.Event) {
	if e.Type == "usage" { // accumulate token usage per model
		if e.Model != "" {
			if _, seen := m.usage[e.Model]; !seen {
				m.usageOrder = append(m.usageOrder, e.Model)
			}
			u := m.usage[e.Model]
			u[0] += e.InTok
			u[1] += e.OutTok
			m.usage[e.Model] = u
		}
		return
	}
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
			if e.Text != "" {
				m.appendLine(idx, todoProgSt.Render("▶ task: ")+short(e.Text, 200))
			}
			m.appendLine(idx, hintSt.Render("● started"))
		} else {
			t.status = statusKey(e.Status)
			m.appendLine(idx, hintSt.Render("■ "+e.Status))
			if m.keepTabs && idx != 0 {
				m.appendLine(idx, hintSt.Render("done — ctrl+w or /close to close this tab"))
			}
		}
	case "text":
		if strings.TrimSpace(e.Text) != "" {
			for _, l := range renderMarkdown(e.Text, m.w) {
				m.appendLine(idx, l)
			}
		}
	case "tool_call":
		if e.Tool == "delegate" {
			m.delegated = true
			for _, l := range delegateLines(e.Args, m.cfg) {
				m.appendLine(idx, l)
			}
			break
		}
		if e.Tool == "update_todos" {
			m.tabs[idx].todos = parseTodos(e.Args) // single list, updated in place
			if idx == 0 {
				m.openTodos = countOpenItems(m.tabs[idx].todos)
			}
			break
		}
		m.appendLine(idx, "→ "+e.Tool+"("+shortArgs(e.Args)+")")
	case "tool_result":
		if e.Tool == "update_todos" {
			break // the checklist is shown as the sticky header
		}
		if e.Tool == "delegate" {
			for _, l := range delegateResultLines(e.Result) {
				m.appendLine(idx, l)
			}
			break
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
	m.relayout() // header height depends on the active tab's todos
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
	var status string
	if m.running {
		elapsed := int(time.Since(m.started).Seconds())
		q := ""
		if len(m.queue) > 0 {
			q = fmt.Sprintf(" · %d queued", len(m.queue))
		}
		status = hintSt.Render(fmt.Sprintf("%s %s (%ds · esc to cancel%s)", m.sp.View(), verb(elapsed), elapsed, q))
	} else {
		status = hintSt.Render("? for shortcuts")
		if m.footer != "" {
			status = m.footer + "   " + status
		}
	}
	left := modeLabel(m.mode) + "   " + status
	hint := left
	if tok := hintSt.Render(m.tokenSummary()); tok != "" {
		gap := m.w - lipgloss.Width(left) - lipgloss.Width(tok)
		if gap < 1 {
			gap = 1
		}
		hint = left + strings.Repeat(" ", gap) + tok
	}
	rule := hintSt.Render(strings.Repeat("─", max(1, m.w)))
	// Banner stays at the top of the scrolling log; the todo checklist is a sticky
	// panel just above the input.
	parts := []string{m.tabBar(), m.vp.View()}
	if todos := todoHeaderLines(m.tabs[m.active].todos); len(todos) > 0 {
		parts = append(parts, strings.Join(todos, "\n"))
	}
	parts = append(parts, rule, m.input.View(), rule, hint)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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

func modeLabel(mode orchestrator.Mode) string {
	sym, color := "⏵⏵", "220" // auto: yellow
	switch mode {
	case orchestrator.ModeChat:
		sym, color = "⏵", "244" // grey
	case orchestrator.ModePlan:
		sym, color = "⏸", "37" // greenish-blue
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(sym + " " + string(mode))
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

func (m model) isLocal(model string) bool {
	check := func(a config.Agent) bool {
		return a.Model == model && a.Provider != "anthropic" &&
			(strings.Contains(a.Endpoint, "localhost") ||
				strings.Contains(a.Endpoint, "127.0.0.1") ||
				strings.Contains(a.Endpoint, "0.0.0.0"))
	}
	if check(m.cfg.Orchestrator) {
		return true
	}
	for _, e := range m.cfg.Executors {
		if check(e) {
			return true
		}
	}
	return false
}

// tokenSummary is the compact persistent counter (e.g. "Σ 12k cloud · 84k local").
func (m model) tokenSummary() string {
	var local, cloud int
	for _, model := range m.usageOrder {
		u := m.usage[model]
		if m.isLocal(model) {
			local += u[0] + u[1]
		} else {
			cloud += u[0] + u[1]
		}
	}
	if local+cloud == 0 {
		return ""
	}
	var parts []string
	if cloud > 0 {
		parts = append(parts, human(cloud)+" cloud")
	}
	if local > 0 {
		parts = append(parts, human(local)+" local")
	}
	return "Σ " + strings.Join(parts, " · ")
}

func human(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%.1fk", float64(n)/1000)
	return strings.Replace(s, ".0k", "k", 1)
}

func (m model) tokenLines() []string {
	if len(m.usageOrder) == 0 {
		return []string{"no token usage yet this session"}
	}
	out := []string{lipgloss.NewStyle().Bold(true).Render("Token usage this session")}
	local := 0
	for _, model := range m.usageOrder {
		u := m.usage[model]
		tag := "cloud"
		if m.isLocal(model) {
			tag = "local · free"
			local += u[0] + u[1]
		}
		out = append(out, fmt.Sprintf("  %-30s in %d · out %d  (%s)", model, u[0], u[1], tag))
	}
	if local > 0 {
		out = append(out, todoDoneSt.Render(fmt.Sprintf("  %d tokens ran on local models at no API cost", local)))
	}
	return out
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
