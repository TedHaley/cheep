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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	"github.com/TedHaley/cheep/internal/approve"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/history"
	"github.com/TedHaley/cheep/internal/orchestrator"
	"github.com/TedHaley/cheep/internal/pricing"
	"github.com/TedHaley/cheep/internal/prompts"
	"github.com/TedHaley/cheep/internal/provider"
)

const bannerArt = ` ██████╗██╗  ██╗███████╗███████╗██████╗ ██╗
██╔════╝██║  ██║██╔════╝██╔════╝██╔══██╗██║
██║     ███████║█████╗  █████╗  ██████╔╝██║
██║     ██╔══██║██╔══╝  ██╔══╝  ██╔═══╝ ╚═╝
╚██████╗██║  ██║███████╗███████╗██║     ██╗
 ╚═════╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝     ╚═╝`

type evMsg core.Event
type doneMsg struct{ r agent.RunResult }
type switchMsg struct {
	cfg config.Config
	ok  bool
	err string
}

type cmdResultMsg struct {
	cmd    string
	stdout string
	stderr string
	exit   int
}
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

	inputHist []string // submitted inputs, for up/down recall
	histIdx   int      // cursor into inputHist (== len means "current draft")
	histDraft string   // the in-progress line saved when you start browsing history

	openTodos    int  // non-done todos from the orchestrator's latest update_todos
	delegated    bool // did the orchestrator call delegate this run?
	nudges       int  // auto-continue count since the last user message
	keepTabs     bool // keep finished executor tabs (else auto-close at turn end)
	budgetWarned bool // already warned at 80% of the budget cap this session
	quitArmed    bool // first ctrl+c pressed while a task was running

	pendingCfg config.Config // last config we tried to switch to (avoid re-verifying)

	connectivity map[string]string // label -> "ok"/"unreachable"/"needs API key"
	welcomeLen   int               // length of the initial banner block (for refresh)

	usage      map[string][2]int // model -> {input, output} tokens this session
	usageOrder []string          // models in first-seen order
	usageRole  map[string][2]int // "orchestrator"/"executor" -> {input, output}

	gate      *approve.Gate     // approval gating for shared-workspace tools
	approvals []approve.Request // pending approval requests (FIFO)

	comp       compState          // input completion dropdown (slash commands, @-files)
	fileList   []string           // workspace files for @-mentions
	promptTpls []prompts.Template // /name prompt templates (project + global)

	// overlay: "" (none) | "help" | "setup" | "setupwiz" | "approval"
	overlay string
	ovTitle string
	ovInput textinput.Model
	ovVP    viewport.Model
	ovSess  *agent.Session
	ovState *configassist.State
	ovLog   []string
	ovBusy  bool

	wiz wizState // discovery configurator (/config and first launch)

	// chat history (persisted to ~/.cheep/history; global timeline)
	histID      string
	histStarted time.Time
	histTitle   string
	histParent  string         // session this one was forked from ("" = root)
	histForkAt  int            // message index in the parent where this branch began
	histList    []history.Meta // populated when the /history or /tree overlay is open
	histDepth   []int          // tree depth per histList row (/tree only)
	histCursor  int

	forkPoints []int // message indices of user turns (the /fork overlay's rows)
	forkCursor int
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
	sepSt       = lipgloss.NewStyle().Foreground(lipgloss.Color("238")) // dim gray for separator
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

// queueHeaderLines renders the queued messages as a sticky header.
func queueHeaderLines(queued []string) []string {
	if len(queued) == 0 {
		return nil
	}
	out := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")).Render("Queued Tasks")}
	for i, q := range queued {
		out = append(out, "  "+hintSt.Render(fmt.Sprintf("%d. %s", i+1, short(q, 100))))
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

// Run starts the full-screen shell. firstRun opens the setup configurator
// immediately (no config yet).
func Run(cfg config.Config, workdir, version string, extraOrch, extraExec []core.Tool, firstRun bool) error {
	Version = version
	events := make(chan core.Event, 1024)
	m := newModel(cfg, workdir, events, extraOrch, extraExec, firstRun)
	// Mouse on for wheel/trackpad scrolling. (To select/copy text, hold Option on
	// macOS or Shift elsewhere to bypass mouse tracking.)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for e := range events {
			p.Send(evMsg(e))
		}
	}()
	go func() {
		for r := range m.gate.Requests {
			p.Send(approvalMsg{r})
		}
	}()
	_, err := p.Run()
	return err
}

func newModel(cfg config.Config, workdir string, events chan core.Event, extraOrch, extraExec []core.Tool, firstRun bool) model {
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

	gateMode, _ := approve.ParseMode(cfg.ApprovalMode)
	m := model{
		cfg:        cfg,
		workdir:    workdir,
		gate:       approve.New(gateMode),
		fileList:   loadFileList(workdir),
		promptTpls: prompts.List(workdir),
		mode:       orchestrator.ModeAuto,
		extraOrch:  extraOrch,
		extraExec:  extraExec,
		keepTabs:   cfg.KeepTabs,
		follow:     true,
		onEvent: func(e core.Event) {
			select {
			case events <- e:
			default: // drop rather than block an agent if the UI falls behind
			}
		},
		tabs:      []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(cfg, nil)}},
		byName:    map[string]int{"orchestrator": 0, "cheep": 0},
		usage:     map[string][2]int{},
		usageRole: map[string][2]int{},
		input:     ta,
		sp:        sp,
		vp:        viewport.New(80, 20),
	}
	m.histStarted = time.Now()
	m.histID = history.NewID(m.histStarted)
	(&m).rebuild(false)
	if firstRun || needsSetup(m.cfg) {
		// No config, the orchestrator can't run, or no executor is configured —
		// cheep needs both roles, so open the configurator to set up the missing
		// piece. Any rescue session stays underneath as a fallback.
		m.overlay = "setupwiz"
		m.wiz = newWizState()
	} else if m.buildErr != nil {
		m.tabs[0].lines = append(m.tabs[0].lines,
			errSt.Render("✗ ")+errText(m.buildErr)+hintSt.Render("  ·  fix with /config or /setup"))
	}
	m.welcomeLen = len(m.tabs[0].lines)
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
// orchestrator tab's opening content (it scrolls away as you work). conn maps a
// label ("orchestrator" / "exec:<name>") to a connectivity status.
func welcomeLines(cfg config.Config, conn map[string]string) []string {
	mark := func(label, base string) string {
		if s := conn[label]; s != "" && s != "ok" {
			return base + "  " + errSt.Render("✗ "+s)
		}
		return base
	}
	info := []string{"orchestrator | " + mark("orchestrator", cfg.Orchestrator.Model)}
	if len(cfg.Executors) == 0 {
		info = append(info, "executors    | "+errSt.Render("none — finish setup in /config"))
	}
	// Collapse interchangeable executors (same model) into one line; the model
	// picks which instance to use, so the type is what matters.
	counts := map[string]int{}
	var order []string
	first := map[string]string{}
	for _, e := range cfg.Executors {
		if counts[e.Model] == 0 {
			order = append(order, e.Model)
			first[e.Model] = e.Name
		}
		counts[e.Model]++
	}
	for _, model := range order {
		info = append(info, "executor     | "+mark("exec:"+first[model], model))
	}
	boxBody := strings.Join(append([]string{
		lipgloss.NewStyle().Bold(true).Render(">_ CHEEP! " + Version),
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
	m.promptTpls = prompts.List(m.workdir) // pick up new/edited templates
	var hist []core.Message
	if keep && m.session != nil {
		hist = m.session.History()
	}
	orch, err := orchestrator.Build(m.cfg, m.workdir, orchestrator.Options{
		Isolate: true, Mode: m.mode, ExtraOrch: m.extraOrch, ExtraExec: m.extraExec, OnEvent: m.onEvent,
		Gate: m.gate,
	})
	m.buildErr = err
	if err != nil {
		m.session = nil
		return
	}
	m.session = orch.Resume(hist)
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, probeCmd(m.cfg)}
	if m.overlay == "setupwiz" { // first launch / unusable orchestrator — scan now
		cmds = append(cmds, wizDiscoverCmd())
	}
	return tea.Batch(cmds...)
}

// orchestratorUsable reports whether the configured orchestrator can actually run.
func orchestratorUsable(cfg config.Config) bool {
	o := cfg.Orchestrator
	if o.Model == "" {
		return false
	}
	if o.Provider == "anthropic" && o.APIKey == "" {
		return false
	}
	return true
}

// needsSetup is true when cheep is missing a usable orchestrator OR any executor.
// cheep always wants both roles configured.
func needsSetup(cfg config.Config) bool {
	return !orchestratorUsable(cfg) || len(cfg.Executors) == 0
}

type probeMsg map[string]string

// probeCmd pings every configured agent so the banner can show connectivity.
func probeCmd(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		res := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		check := func(label string, a config.Agent) {
			defer wg.Done()
			st := "ok"
			switch {
			case a.Model == "":
				st = "no model"
			case a.Provider == "anthropic" && a.APIKey == "":
				st = "needs API key"
			default:
				p := provider.For(a.Provider, a.Endpoint, a.APIKey, 16)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := p.Complete(ctx, a.Model, "ok", []core.Message{{Role: "user", Text: "ok"}}, nil)
				cancel()
				if err != nil {
					st = "unreachable"
				}
			}
			mu.Lock()
			res[label] = st
			mu.Unlock()
		}
		wg.Add(1)
		go check("orchestrator", cfg.Orchestrator)
		for _, e := range cfg.Executors {
			wg.Add(1)
			go check("exec:"+e.Name, e)
		}
		wg.Wait()
		return probeMsg(res)
	}
}

// bashCmd runs a shell command and returns the result as a cmdResultMsg.
func bashCmd(cmdStr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		var stdout, stderr strings.Builder
		c.Stdout = &stdout
		c.Stderr = &stderr
		err := c.Run()
		exit := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exit = exitErr.ExitCode()
			} else {
				exit = 1
			}
		}
		return cmdResultMsg{
			cmd:    cmdStr,
			stdout: strings.TrimSpace(stdout.String()),
			stderr: strings.TrimSpace(stderr.String()),
			exit:   exit,
		}
	}
}

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
	m.input.CursorEnd()
	header := 0
	if len(m.tabs) > 0 {
		// Account for todos
		todoLines := todoHeaderLines(m.tabs[m.active].todos)
		if len(todoLines) > 0 {
			header += len(todoLines)
		}
		// Account for queue header
		if m.running && len(m.queue) > 0 {
			queueLines := queueHeaderLines(m.queue)
			if len(queueLines) > 0 {
				header += len(queueLines)
			}
		}
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

	case approvalMsg:
		return m.pushApproval(msg.req)

	case doneMsg:
		m.running = false
		m.cancel = nil
		m.quitArmed = false
		// A finished/cancelled run can leave queued approvals nobody is
		// waiting on (the gated calls unblocked via ctx.Done). Drop them.
		if len(m.approvals) > 0 {
			for _, r := range m.approvals {
				select {
				case r.Resp <- approve.Deny:
				default:
				}
			}
			m.approvals = nil
			if m.overlay == "approval" {
				m.overlay = ""
			}
		}
		m.tabs[0].status = statusKey(msg.r.Status)
		m.footer = fmt.Sprintf("%s · %d turns · %s in / %s out (this turn)", msg.r.Status, msg.r.Turns, human(msg.r.InputTokens), human(msg.r.OutputTokens))
		if m.keepTabs {
			if len(m.tabs) > 1 {
				m.footer += "   tab → executor, ctrl+w to close"
			}
		} else {
			(&m).closeFinishedTabs()
		}
		(&m).syncViewport()
		(&m).saveHistory() // persist the conversation after every completed turn
		// The orchestrator may have rewired cheep via the config tools. Verify the
		// new orchestrator is reachable BEFORE switching to it (don't brick the
		// session); applied in the switchMsg handler.
		if fresh, err := config.Load(); err == nil && !configEqual(fresh, m.cfg) && !configEqual(fresh, m.pendingCfg) {
			m.pendingCfg = fresh
			m.footer = "verifying new orchestrator " + fresh.Orchestrator.Model + " …"
			return m, verifyConfigCmd(fresh)
		}
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
			if m.overBudget() {
				m.footer = "budget cap reached — raise with /budget to run queued messages"
				return m, nil
			}
			next := m.queue[0]
			m.queue = m.queue[1:]
			m.nudges, m.openTodos = 0, 0
			return m.startTask(next)
		}
		mark := "✓"
		if msg.r.Status != "completed" {
			mark = "⚠"
		}
		return m, tea.SetWindowTitle("cheep " + mark + " " + msg.r.Status + " — " + filepath.Base(m.workdir))

	case ovDoneMsg:
		m.ovBusy = false
		return m, nil

	case wizMsg:
		m.wiz.loading = false
		m.wiz.cands = msg.cands
		m.wiz.extra = msg.extra
		return m, nil

	case probeMsg:
		m.connectivity = map[string]string(msg)
		if len(m.tabs) > 0 && len(m.tabs[0].lines) == m.welcomeLen { // banner untouched
			m.tabs[0].lines = welcomeLines(m.cfg, m.connectivity)
			m.welcomeLen = len(m.tabs[0].lines)
			(&m).syncViewport()
		}
		return m, nil

	case cmdResultMsg:
		m.running = false
		if msg.exit == 0 {
			m.tabs[0].status = "ok"
			m.footer = "command exited 0"
		} else {
			m.tabs[0].status = "err"
			m.footer = "command exited " + strconv.Itoa(msg.exit)
		}
		(&m).appendLine(0, hintSt.Render("stdout:"))
		if msg.stdout != "" {
			for _, l := range strings.Split(msg.stdout, "\n") {
				(&m).appendLine(0, "  "+l)
			}
		}
		if msg.stderr != "" {
			(&m).appendLine(0, errSt.Render("stderr:"))
			for _, l := range strings.Split(msg.stderr, "\n") {
				(&m).appendLine(0, "  "+l)
			}
		}
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	case switchMsg:
		if msg.ok {
			m.cfg = msg.cfg
			m.keepTabs = msg.cfg.KeepTabs
			(&m).rebuild(true) // new providers, conversation preserved
			(&m).appendLine(0, "")
			for _, l := range welcomeLines(msg.cfg, m.connectivity) { // re-show banner with new models
				(&m).appendLine(0, l)
			}
			m.footer = "✓ switched orchestrator to " + msg.cfg.Orchestrator.Model
			m.follow = true
			(&m).syncViewport()
		} else {
			m.footer = "couldn't switch to " + msg.cfg.Orchestrator.Model + ": " + msg.err + " — keeping current orchestrator"
		}
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
		// The completion dropdown captures navigation keys while open.
		if len(m.comp.opts) > 0 {
			switch msg.String() {
			case "up":
				m.comp.idx = (m.comp.idx - 1 + len(m.comp.opts)) % len(m.comp.opts)
				return m, nil
			case "down":
				m.comp.idx = (m.comp.idx + 1) % len(m.comp.opts)
				return m, nil
			case "tab":
				(&m).acceptCompletion()
				(&m).updateCompletions()
				(&m).relayout()
				return m, nil
			case "enter":
				if (&m).acceptCompletion() == "slash" {
					return m.submit() // run the chosen command immediately
				}
				(&m).relayout()
				return m, nil
			case "esc":
				m.comp = compState{}
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c":
			// Don't tear down mid-run on a single keypress: agents may hold
			// work that only lands at the merge boundary.
			if m.running && !m.quitArmed {
				m.quitArmed = true
				m.footer = "a task is running — ctrl+c again to quit anyway (esc cancels the task first)"
				return m, nil
			}
			(&m).saveHistory()
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
		case "up":
			// Recall previous input when the cursor is on the first line;
			// otherwise fall through to normal cursor movement in a multi-line draft.
			if m.input.Line() == 0 && len(m.inputHist) > 0 {
				(&m).historyPrev()
				(&m).relayout()
				return m, nil
			}
		case "down":
			if m.input.Line() == m.input.LineCount()-1 && m.histIdx < len(m.inputHist) {
				(&m).historyNext()
				(&m).relayout()
				return m, nil
			}
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
		(&m).updateCompletions()
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

// historyPrev recalls an older input; historyNext moves toward the live draft.
func (m *model) historyPrev() {
	if len(m.inputHist) == 0 {
		return
	}
	if m.histIdx >= len(m.inputHist) {
		m.histDraft = m.input.Value() // save the in-progress line before browsing
	}
	if m.histIdx > 0 {
		m.histIdx--
	}
	m.input.SetValue(m.inputHist[m.histIdx])
	m.input.CursorEnd()
}

func (m *model) historyNext() {
	if m.histIdx >= len(m.inputHist) {
		return
	}
	m.histIdx++
	if m.histIdx >= len(m.inputHist) {
		m.input.SetValue(m.histDraft)
	} else {
		m.input.SetValue(m.inputHist[m.histIdx])
	}
	m.input.CursorEnd()
}

func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	// Record in history (skip consecutive duplicates) and reset the cursor to "new".
	if n := len(m.inputHist); n == 0 || m.inputHist[n-1] != text {
		m.inputHist = append(m.inputHist, text)
	}
	m.histIdx = len(m.inputHist)
	m.histDraft = ""
	m.input.Reset()
	(&m).relayout()
	if strings.HasPrefix(text, "/") {
		return m.slash(text)
	}
	// Handle !<command> for inline shell execution (Claude Code style)
	if strings.HasPrefix(text, "!") {
		cmdStr := strings.TrimSpace(strings.TrimPrefix(text, "!"))
		if cmdStr == "" {
			m.footer = "usage: !<command>"
			return m, nil
		}
		(&m).appendLine(0, hintSt.Render("🔧 "+cmdStr))
		m.running = true
		m.tabs[0].status = "run"
		m.active = 0
		m.follow = true
		m.started = time.Now()
		return m, tea.Batch(m.sp.Tick, bashCmd(cmdStr))
	}
	return m.sendUser(text, userSt.Render("› "+text))
}

// sendUser routes a user message to the orchestrator with the usual guards
// (no session, task already running, over budget). display is the line shown
// in the log — for prompt templates it is the typed /name, not the expansion.
func (m model) sendUser(text, display string) (tea.Model, tea.Cmd) {
	if m.session == nil {
		// Show the message and a clear, actionable error in the conversation.
		(&m).appendLine(0, display)
		(&m).appendLine(0, errSt.Render("✗ can't run")+hintSt.Render(" — "+errText(m.buildErr)+"  ·  fix with /config or /setup"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	if m.running {
		// Queue it — it runs automatically when the current task finishes.
		m.queue = append(m.queue, text)
		(&m).appendLine(0, display+"  "+hintSt.Render("(queued)"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	if m.overBudget() {
		(&m).appendLine(0, display)
		(&m).appendLine(0, errSt.Render("✗ over budget")+hintSt.Render(fmt.Sprintf(
			" — %s of %s spent; raise or clear the cap with /budget", usd(m.spent()), usd(m.budget()))))
		m.active, m.follow = 0, true
		(&m).syncViewport()
		return m, nil
	}
	m.nudges = 0
	m.openTodos = 0
	m.tabs[0].todos = nil // dismiss the previous task's checklist
	return m.runMessage(text, display)
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
	return m, tea.Batch(m.sp.Tick, run,
		tea.SetWindowTitle("cheep ● working — "+filepath.Base(m.workdir)))
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
		(&m).saveHistory() // keep the conversation we're clearing
		m.tabs = []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(m.cfg, m.connectivity)}}
		m.welcomeLen = len(m.tabs[0].lines)
		m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
		m.active = 0
		(&m).rebuild(false)
		// start a fresh history session (a new root, not a fork)
		m.histStarted = time.Now()
		m.histID = history.UniqueID(m.histStarted)
		m.histTitle = ""
		m.histParent, m.histForkAt = "", 0
		m.footer = "(cleared)"
		(&m).syncViewport()
	case "/history", "/resume":
		return m.openHistory()
	case "/fork":
		return m.openFork()
	case "/tree":
		return m.openTree()
	case "/approval", "/approvals":
		arg := ""
		if f := strings.Fields(text); len(f) > 1 {
			arg = f[1]
		}
		return m.setApprovalMode(arg)
	case "/model":
		f := strings.Fields(text)
		if len(f) < 2 {
			m.footer = "orchestrator model: " + m.cfg.Orchestrator.Model + " — /model <name> switches (conversation is kept)"
			return m, nil
		}
		m.cfg.Orchestrator.Model = f[1]
		if err := config.Save(m.cfg); err != nil {
			m.footer = "model not saved: " + err.Error()
			return m, nil
		}
		m.pendingCfg = m.cfg // skip the config-change re-verification round-trip
		(&m).rebuild(true)   // history is threaded through, so context survives
		if m.buildErr != nil {
			m.footer = "model switch failed: " + errText(m.buildErr)
		} else {
			m.footer = "orchestrator now on " + f[1]
		}
	case "/delivery":
		f := strings.Fields(text)
		if len(f) < 2 {
			mode := m.cfg.Delivery
			if mode == "" {
				mode = "merge"
			}
			m.footer = "delivery: " + mode + " — /delivery merge|pr switches how validated work lands"
			return m, nil
		}
		switch f[1] {
		case "merge", "pr":
			m.cfg.Delivery = f[1]
			if f[1] == "merge" {
				m.cfg.Delivery = "" // default, keep config.json clean
			}
			_ = config.Save(m.cfg)
			(&m).rebuild(true)
			if f[1] == "pr" {
				m.footer = "delivery: pr — validated subtask branches are pushed and opened as PRs (needs git remote + gh)"
			} else {
				m.footer = "delivery: merge — validated subtask branches merge into the local checkout"
			}
		default:
			m.footer = "usage: /delivery merge|pr"
		}
	case "/budget":
		f := strings.Fields(text)
		switch {
		case len(f) < 2:
			if b := m.budget(); b > 0 {
				m.footer = fmt.Sprintf("budget: %s of %s used (this project)", usd(m.spent()), usd(b))
			} else {
				m.footer = "no budget cap for this project — set one with /budget 5  (or /budget off)"
			}
		case f[1] == "off" || f[1] == "none" || f[1] == "0":
			if m.cfg.Budgets != nil {
				delete(m.cfg.Budgets, m.workdir)
			}
			m.budgetWarned = false
			_ = config.Save(m.cfg)
			m.footer = "budget cap cleared for this project"
		default:
			if v, err := strconv.ParseFloat(strings.TrimPrefix(f[1], "$"), 64); err == nil && v > 0 {
				if m.cfg.Budgets == nil {
					m.cfg.Budgets = map[string]float64{}
				}
				m.cfg.Budgets[m.workdir], m.budgetWarned = v, false
				_ = config.Save(m.cfg)
				m.footer = "budget cap set to " + usd(v) + " for this project"
			} else {
				m.footer = "usage: /budget <dollars> | off"
			}
		}
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
	case "/config":
		return m.openWizard()
	case "/setup":
		return m.openSetup()
	case "/help", "/?":
		m.overlay = "help"
	case "/prompts":
		if len(m.promptTpls) == 0 {
			m.footer = "no prompt templates — drop markdown files in .cheep/prompts/ or ~/.cheep/prompts/"
			return m, nil
		}
		m.appendLine(m.active, hintSt.Render("prompt templates (invoke as /name [args]):"))
		for _, t := range m.promptTpls {
			line := "  /" + t.Name
			if t.Description != "" {
				line += hintSt.Render("  — " + t.Description)
			}
			m.appendLine(m.active, line)
		}
		m.follow = true
		(&m).syncViewport()
	default:
		name := strings.TrimPrefix(strings.Fields(text)[0], "/")
		if t, ok := prompts.Find(m.workdir, name); ok {
			args := strings.TrimSpace(strings.TrimPrefix(text, "/"+name))
			return m.sendUser(prompts.Expand(t, args), userSt.Render("› "+text))
		}
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

// budget is the active cap for the current project (or the global default).
func (m model) budget() float64 { return m.cfg.Budget(m.workdir) }

// overBudget reports whether estimated session spend has hit the cap.
func (m model) overBudget() bool {
	b := m.budget()
	return b > 0 && m.spent() >= b
}

// enforceBudget warns at 80% of the cap and cancels a running task at 100%.
func (m *model) enforceBudget() {
	b := m.budget()
	if b <= 0 {
		return
	}
	sp := m.spent()
	switch {
	case sp >= b:
		if m.running && m.cancel != nil {
			m.cancel()
			m.footer = errSt.Render(fmt.Sprintf("✗ budget cap %s reached (%s) — stopping; raise with /budget",
				usd(b), usd(sp)))
		}
	case !m.budgetWarned && sp >= 0.8*b:
		m.budgetWarned = true
		m.footer = fmt.Sprintf("⚠ %s of %s budget used", usd(sp), usd(b))
	}
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
	if e.Type == "usage" { // accumulate token usage per model and per role
		if e.Model != "" {
			if _, seen := m.usage[e.Model]; !seen {
				m.usageOrder = append(m.usageOrder, e.Model)
			}
			u := m.usage[e.Model]
			u[0] += e.InTok
			u[1] += e.OutTok
			m.usage[e.Model] = u

			role := "executor"
			if e.Agent == "orchestrator" || e.Agent == "cheep" {
				role = "orchestrator"
			}
			r := m.usageRole[role]
			r[0] += e.InTok
			r[1] += e.OutTok
			m.usageRole[role] = r
		}
		m.enforceBudget()
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
			line := glyph(t.status) + " " + describeStatus(e.Status)
			if idx != 0 { // executor finished — show how to close its tab
				line += "  ·  ctrl+w or /close to close this tab"
			}
			m.appendLine(idx, hintSt.Render(line))
		}
	case "text":
		if strings.TrimSpace(e.Text) != "" {

			// Add a visual separator between user input and assistant response
			m.appendLine(idx, sepSt.Render(strings.Repeat("─", max(1, m.w-2))))
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
	// Show queued tasks below todos when running
	if m.running && len(m.queue) > 0 {
		queueLines := queueHeaderLines(m.queue)
		if len(queueLines) > 0 {
			parts = append(parts, strings.Join(queueLines, "\n"))
		}
	}
	if comp := m.viewCompletions(); comp != "" {
		parts = append(parts, comp)
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

// verifyConfigCmd pings the new orchestrator; the result drives the switch.
func verifyConfigCmd(cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		o := cfg.Orchestrator
		if o.Provider == "anthropic" && o.APIKey == "" {
			return switchMsg{cfg: cfg, ok: false, err: "no API key (set ANTHROPIC_API_KEY in ~/.cheep/keys.env)"}
		}
		if o.Model == "" {
			return switchMsg{cfg: cfg, ok: false, err: "no model set"}
		}
		p := provider.For(o.Provider, o.Endpoint, o.APIKey, 16)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if _, err := p.Complete(ctx, o.Model, "Reply with: ok", []core.Message{{Role: "user", Text: "ok"}}, nil); err != nil {
			return switchMsg{cfg: cfg, ok: false, err: short(err.Error(), 120)}
		}
		return switchMsg{cfg: cfg, ok: true}
	}
}

func configEqual(a, b config.Config) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func describeStatus(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "max_turns":
		return "stopped early — hit the turn limit"
	case "looping":
		return "stopped early — detected a repeating loop"
	case "context_exhausted":
		return "stopped early — ran out of context budget"
	case "timeout":
		return "stopped early — timed out"
	case "aborted":
		return "cancelled"
	case "error":
		return "error"
	}
	return s
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
	for _, a := range append([]config.Agent{m.cfg.Orchestrator}, m.cfg.Executors...) {
		if a.Model == model && pricing.IsLocal(a.Provider, a.Endpoint) {
			return true
		}
	}
	return false
}

// rate returns the per-1M-token input/output price for a model and a kind:
// "local" (free), "priced" (known/overridden), or "unknown" (cloud, unpriced).
func (m model) rate(modelName string) (inR, outR float64, kind string) {
	if m.isLocal(modelName) {
		return 0, 0, "local"
	}
	for _, a := range append([]config.Agent{m.cfg.Orchestrator}, m.cfg.Executors...) {
		if a.Model == modelName && (a.PriceIn > 0 || a.PriceOut > 0) {
			return a.PriceIn, a.PriceOut, "priced"
		}
	}
	if i, o, ok := pricing.Rate(modelName); ok {
		return i, o, "priced"
	}
	return 0, 0, "unknown"
}

func cost(in, out int, inR, outR float64) float64 {
	return pricing.Cost(in, out, inR, outR)
}

// referenceRate is the cloud price used to value local/free tokens for the
// savings estimate: the orchestrator's rate if it's cloud, else Claude Sonnet.
func (m model) referenceRate() (inR, outR float64, label string) {
	if i, o, k := m.rate(m.cfg.Orchestrator.Model); k == "priced" {
		return i, o, m.cfg.Orchestrator.Model
	}
	return 3, 15, "claude-sonnet"
}

func usd(v float64) string {
	if v > 0 && v < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// spent totals the estimated USD cost of priced (cloud) usage this session.
func (m model) spent() float64 {
	var total float64
	for _, model := range m.usageOrder {
		if inR, outR, k := m.rate(model); k == "priced" {
			u := m.usage[model]
			total += cost(u[0], u[1], inR, outR)
		}
	}
	return total
}

// tokenSummary is the compact persistent counter (e.g. "Σ orch 13k · exec 81k · $0.07").
func (m model) tokenSummary() string {
	o := m.usageRole["orchestrator"]
	e := m.usageRole["executor"]
	ot, et := o[0]+o[1], e[0]+e[1]
	if ot+et == 0 {
		return ""
	}
	var parts []string
	if ot > 0 {
		parts = append(parts, "orch "+human(ot))
	}
	if et > 0 {
		parts = append(parts, "exec "+human(et))
	}
	s := "Σ " + strings.Join(parts, " · ")
	spent := m.spent()
	switch b := m.budget(); {
	case b > 0:
		s += " · " + usd(spent) + "/" + usd(b)
	case spent > 0:
		s += " · " + usd(spent)
	}
	return s
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
	for _, role := range []string{"orchestrator", "executor"} {
		u := m.usageRole[role]
		if u[0]+u[1] == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("  %-13s in %s · out %s", role, human(u[0]), human(u[1])))
	}
	out = append(out, "")
	var spent, freeTokens, savings float64
	refIn, refOut, refLabel := m.referenceRate()
	for _, model := range m.usageOrder {
		u := m.usage[model]
		inR, outR, kind := m.rate(model)
		var note string
		switch kind {
		case "local":
			note = todoDoneSt.Render("free · local")
			freeTokens += float64(u[0] + u[1])
			savings += cost(u[0], u[1], refIn, refOut)
		case "priced":
			c := cost(u[0], u[1], inR, outR)
			spent += c
			note = "~" + usd(c)
		default:
			note = hintSt.Render("cloud · unpriced")
		}
		out = append(out, fmt.Sprintf("  %-30s in %s · out %s  %s", model, human(u[0]), human(u[1]), note))
	}
	out = append(out, "")
	out = append(out, lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("≈ %s spent this session", usd(spent))))
	if freeTokens > 0 {
		out = append(out, todoDoneSt.Render(fmt.Sprintf(
			"   %s tokens ran free on local models — ≈ %s saved at %s rates",
			human(int(freeTokens)), usd(savings), refLabel)))
	}
	if b := m.budget(); b > 0 {
		line := fmt.Sprintf("   budget cap: %s of %s used (this project)", usd(spent), usd(b))
		if m.overBudget() {
			out = append(out, errSt.Render(line+" — reached"))
		} else {
			out = append(out, hintSt.Render(line))
		}
	}
	out = append(out, hintSt.Render("   estimates only — set price_in/price_out per agent to tune"))
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
