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
	"sort"
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
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/reflow/wordwrap"

	"github.com/TedHaley/cheep/internal/agent"
	"github.com/TedHaley/cheep/internal/approve"
	"github.com/TedHaley/cheep/internal/capabilities"
	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/history"
	"github.com/TedHaley/cheep/internal/inflight"
	"github.com/TedHaley/cheep/internal/jobs"
	"github.com/TedHaley/cheep/internal/orchestrator"
	"github.com/TedHaley/cheep/internal/plugins"
	"github.com/TedHaley/cheep/internal/pricing"
	"github.com/TedHaley/cheep/internal/prompts"
	"github.com/TedHaley/cheep/internal/provider"
	"github.com/TedHaley/cheep/internal/update"
)

const bannerArt = ` ██████╗██╗  ██╗███████╗███████╗██████╗ ██╗
██╔════╝██║  ██║██╔════╝██╔════╝██╔══██╗██║
██║     ███████║█████╗  █████╗  ██████╔╝██║
██║     ██╔══██║██╔══╝  ██╔══╝  ██╔═══╝ ╚═╝
╚██████╗██║  ██║███████╗███████╗██║     ██╗
 ╚═════╝╚═╝  ╚═╝╚══════╝╚══════╝╚═╝     ╚═╝`

type evMsg core.Event
type doneMsg struct{ r agent.RunResult }
type suggestionsMsg struct{ sug []string }
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

	vp        viewport.Model
	input     textarea.Model
	sp        spinner.Model
	w, h      int
	ready     bool
	running   bool
	started   time.Time
	cancel    context.CancelFunc
	queue     []string // messages typed while a task is running
	footer    string
	updateVer string // newer release the launch check found ("" if none/current)

	// In-app text selection over the conversation viewport (full-screen mode,
	// mouse captured). Rows are viewport-relative (0 = first visible line).
	selecting bool
	selY0     int
	selY1     int

	// auto-improve: the last user task (for gap detection) and a capability the
	// detector suggested that's awaiting /autoimprove install.
	lastTask   string
	pendingCap capabilities.Capability

	lastKeyAt time.Time // previous keypress time — sub-10ms Enters are paste bursts, not submits

	inputHist []string // submitted inputs, for up/down recall
	histIdx   int      // cursor into inputHist (== len means "current draft")
	histDraft string   // the in-progress line saved when you start browsing history

	openTodos    int      // non-done todos from the orchestrator's latest update_todos
	delegated    bool     // did the orchestrator call delegate this run?
	stowing      bool     // current run is a /stow; capture its reply as a run note
	mouseOn      bool     // mouse capture opt-in: wheel scrolls tabs, selection needs Option/Shift
	inline       bool     // render inline (chat in terminal scrollback) instead of the alt-screen tab UI
	pendingOut   []string // inline: lines queued for tea.Println into the scrollback
	nudges       int      // auto-continue count since the last user message
	keepTabs     bool     // keep finished executor tabs (else auto-close at turn end)
	budgetWarned bool     // already warned at 80% of the budget cap this session
	quitArmed    bool     // first ctrl+c pressed while a task was running

	pendingCfg config.Config // last config we tried to switch to (avoid re-verifying)

	connectivity map[string]string // label -> "ok"/"unreachable"/"needs API key"
	welcomeLen   int               // length of the initial banner block (for refresh)
	welcomeShown bool              // inline: banner has been printed to scrollback (once, at known width)

	ctxTokens  int               // orchestrator conversation size estimate (context bar)
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

	jobsList   []jobs.Job // /scheduled overlay rows
	jobsCursor int

	suggestions   []string           // next-step suggestions from the last reply; [0] shown as input ghost text (Tab accepts)
	suggestCancel context.CancelFunc // aborts an in-flight async suggestion call
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
	// userMsgSt tints your own messages with a faint grey block so they stand
	// out from the agent's default-background replies (Claude-style).
	userMsgSt  = lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("236"))
	todoDoneSt = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	todoProgSt = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	sepSt      = lipgloss.NewStyle().Foreground(lipgloss.Color("238")) // dim gray for separator
	selHiSt    = lipgloss.NewStyle().Reverse(true)                     // click-drag selection highlight
)

// userLine renders a user message for the log, wrapped to the window width —
// the tab viewport does not wrap, so an unwrapped long message runs off-page.
func userLine(text string, w int) string {
	if w < 20 {
		w = 80
	}
	// Pad every wrapped row to a uniform width so the grey tint reads as one
	// clean block rather than a ragged highlight around the glyphs.
	width := w - 2
	lines := strings.Split(wordwrap.String("› "+text, width), "\n")
	for i, l := range lines {
		lines[i] = userMsgSt.Width(width).Render(l)
	}
	return strings.Join(lines, "\n")
}

// splitSuggestions pulls a trailing "[[NEXT]] a | b | c" line (emitted by the
// orchestrator) off the reply, returning the cleaned text and up to 3 chips.
func splitSuggestions(text string) (string, []string) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(t, "[[NEXT]]"); ok {
			var sug []string
			for _, s := range strings.Split(rest, "|") {
				if s = strings.TrimSpace(s); s != "" {
					sug = append(sug, s)
				}
			}
			if len(sug) > 3 {
				sug = sug[:3]
			}
			clean := strings.TrimRight(strings.Join(append(lines[:i], lines[i+1:]...), "\n"), "\n")
			return clean, sug
		}
		break // sentinel is only honored as the last non-empty line
	}
	return text, nil
}

// suggestCmd generates next-step suggestions async on the cheapest executor
// (local = free), for when the model didn't emit an inline [[NEXT]] line. It
// never blocks the user; chips appear when it returns.
func (m model) suggestCmd(ctx context.Context, reply string) tea.Cmd {
	if m.cfg.SuggestOff || strings.TrimSpace(reply) == "" {
		return nil
	}
	a := m.cfg.Orchestrator // cheapest agent, preferring free/local
	for _, e := range m.cfg.Executors {
		if pricing.Score(e) < pricing.Score(a) {
			a = e
		}
	}
	// Generous max_tokens: reasoning models burn budget thinking before they
	// emit the answer, so a small cap yields empty content.
	prov := provider.For(a.Provider, a.Endpoint, a.APIKey, 2048)
	model := a.Model
	reply = short(reply, 1500)
	return func() tea.Msg {
		sys := "You propose next steps. Given the assistant's last reply to the user, output up to 3 " +
			"SHORT next-step actions the user could take, each 3–7 words, imperative (e.g. 'run the tests', " +
			"'ship it as a PR'). Output ONLY the actions separated by ' | ' and nothing else. If no useful " +
			"next step exists, output exactly: NONE"
		turn, err := prov.Complete(ctx, model, sys, []core.Message{{Role: "user", Text: "Assistant's last reply:\n" + reply}}, nil)
		if err != nil {
			return suggestionsMsg{nil}
		}
		txt := strings.TrimSpace(turn.Message.Text)
		if txt == "" || strings.EqualFold(txt, "NONE") {
			return suggestionsMsg{nil}
		}
		var sug []string
		for _, s := range strings.Split(txt, "|") {
			if s = strings.TrimSpace(s); s != "" && !strings.EqualFold(s, "NONE") {
				sug = append(sug, s)
			}
		}
		if len(sug) > 3 {
			sug = sug[:3]
		}
		return suggestionsMsg{sug}
	}
}

// defaultPlaceholder is the empty-box hint shown when there's no suggestion.
const defaultPlaceholder = "type a task, or /help"

// refreshPlaceholder shows the top next-step suggestion as ghost text inside the
// input box (accept with Tab), Claude-style — or the default hint when there's
// none. The textarea only renders the placeholder while the box is empty, so it
// naturally disappears the moment you start typing.
func (m *model) refreshPlaceholder() {
	if len(m.suggestions) > 0 {
		m.input.Placeholder = m.suggestions[0]
	} else {
		m.input.Placeholder = defaultPlaceholder
	}
}

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
	out := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4")).Render("Queued Tasks") +
		hintSt.Render("  (/queue rm N to remove)")}
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
	// Full-screen tab-per-agent UI by default; "inline": true opts into
	// Claude Code-style scrollback rendering instead. In full-screen, mouse
	// capture is on so the wheel scrolls the focused tab (/mouse releases it
	// for text selection), and the terminal's alternate-scroll translation is
	// disabled so a released wheel can never spray arrow keys into the input.
	// Wipe the terminal's scrollback (3J) + screen (2J) before anything draws.
	// In full-screen mode with mouse capture off, the wheel scrolls the terminal
	// (not cheep), so without this you'd scroll up into stale main-buffer
	// content. Done before bubbletea enters the alt screen, so it clears the
	// main buffer.
	os.Stdout.WriteString("\x1b[3J\x1b[2J\x1b[H")

	var opts []tea.ProgramOption
	if !cfg.Inline {
		opts = append(opts, tea.WithAltScreen())
		if !cfg.MouseOff {
			opts = append(opts, tea.WithMouseCellMotion())
		}
		os.Stdout.WriteString("\x1b[?1007l")       // no wheel→arrow translation
		defer os.Stdout.WriteString("\x1b[?1007h") // restore the common default
	}
	// Enable the kitty keyboard protocol (the same mechanism Claude Code uses)
	// so the terminal reports Shift+Enter and friends, and feed input through a
	// reader that translates those sequences into forms Bubble Tea v1
	// understands. See internal/tui/keyboard.go.
	os.Stdout.WriteString(kbEnable)
	defer os.Stdout.WriteString(kbDisable)
	opts = append(opts, tea.WithInput(newTranslatingReader(os.Stdin)))

	p := tea.NewProgram(m, opts...)
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
	ta.Placeholder = defaultPlaceholder
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
	if cfg.NoMistakes {
		gateMode = approve.ModeApprove // no-mistakes implies the strictest gate
	}
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
		mouseOn:    !cfg.MouseOff,
		inline:     cfg.Inline,
		follow:     true,
		onEvent: func(e core.Event) {
			select {
			case events <- e:
			default: // drop rather than block an agent if the UI falls behind
			}
		},
		tabs:      []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(cfg, nil, 0)}},
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
	// Surface delegations a previous cheep process left in flight (crash/kill):
	// any work they produced survives on quarantined worktree branches.
	for _, j := range inflight.Stale(workdir) {
		when := j.Started.Local().Format("Jan 2 15:04")
		sub := short(strings.ReplaceAll(j.Subtask, "\n", " "), 80)
		var line string
		switch {
		case j.Kind == "scout":
			// A scout discards its changes by design — nothing is stranded, it
			// just didn't finish. Informational, not alarming.
			line = hintSt.Render("• interrupted research (" + when + ", " + j.Executor + "): " + sub + "  · re-run if you still need it")
		case j.Branch != "":
			line = errSt.Render("⚠ interrupted work") + hintSt.Render(" ("+when+", "+j.Executor+"): "+sub+
				"  · partial work is on branch "+j.Branch+"  · see `cheep worktree list`")
		default:
			line = errSt.Render("⚠ interrupted work") + hintSt.Render(" ("+when+", "+j.Executor+"): "+sub+
				"  · ran in the shared workspace — check `git status`")
		}
		m.tabs[0].lines = append(m.tabs[0].lines, line)
	}
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
func welcomeLines(cfg config.Config, conn map[string]string, w int) []string {
	if w <= 0 {
		w = 80
	}
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

	banner := strings.Join(gradientBanner(), "\n")
	header := lipgloss.JoinHorizontal(lipgloss.Center, banner, "   ", box)
	if lipgloss.Width(header) > w {
		// Too wide to sit side by side — stack the box under the banner so the
		// lines stay within the terminal width.
		header = lipgloss.JoinVertical(lipgloss.Left, banner, "", box)
	}

	out := []string{""} // top space
	out = append(out, strings.Split(header, "\n")...)
	out = append(out, "",
		hintSt.Render("Tips: tab / ⌥tab switches agents (watch executors, reviewer) · shift+tab cycles modes · /help"), "")
	// Hard guard: never emit a line wider than the terminal. An over-wide line
	// soft-wraps in the scrollback and garbles when the pane is later resized.
	for i, l := range out {
		if lipgloss.Width(l) > w {
			out[i] = ansi.Truncate(l, w, "")
		}
	}
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

// disableAltScroll turns off the terminal's alternate-scroll mode (DEC 1007),
// which translates the wheel into arrow keys. Without this, a wheel-up in the
// full-screen UI arrives as ↑ and recalls the previous input instead of
// scrolling the log. cheep disables it before the program starts, but entering
// the alternate screen re-enables it on many terminals, so we reassert it once
// the program is up (and again after /mouse toggles).
func disableAltScroll() tea.Msg {
	os.Stdout.WriteString("\x1b[?1007l")
	return nil
}

func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, probeCmd(m.cfg), checkUpdateCmd()}
	if !m.inline {
		cmds = append(cmds, disableAltScroll)
	}
	// The inline banner is printed on the first WindowSizeMsg instead of here:
	// it must be built at the real terminal width, or its wide art soft-wraps in
	// the scrollback and garbles on later resizes/scroll.
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

// updateAvailableMsg is posted by the silent launch check when a newer release
// exists. upgradeDoneMsg reports the outcome of an on-demand /upgrade.
type updateAvailableMsg struct{ latest string }
type upgradeDoneMsg struct {
	via     string // "brew" or "binary"
	from    string
	to      string
	already bool // already on the latest
	output  string
	err     error
}

// checkUpdateCmd quietly asks GitHub for the latest release and, only if it's
// newer than the running build, posts updateAvailableMsg. Any error (offline,
// rate-limited) is swallowed — a version check must never disrupt startup.
func checkUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		rel, err := update.Latest(ctx)
		if err != nil || !update.IsNewer(Version, rel.Version) {
			return nil
		}
		return updateAvailableMsg{latest: rel.Version}
	}
}

// upgradeCmd checks for a newer release and installs it (via brew or a binary
// self-replace). Runs off the UI goroutine; posts upgradeDoneMsg when finished.
func upgradeCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		rel, err := update.Latest(ctx)
		if err != nil {
			return upgradeDoneMsg{err: err}
		}
		if !update.IsNewer(Version, rel.Version) {
			return upgradeDoneMsg{already: true, to: rel.Version}
		}
		res, err := update.Upgrade(ctx, rel.Version)
		if err != nil {
			return upgradeDoneMsg{to: rel.Version, output: res.Output, err: err}
		}
		return upgradeDoneMsg{via: res.Via, from: Version, to: rel.Version, output: res.Output}
	}
}

// improveSuggestMsg carries an auto-improve capability suggestion from the
// post-turn gap detector.
type improveSuggestMsg struct {
	cap capabilities.Capability
	ok  bool
}

// detectImproveCmd runs the auto-improve gap detector on the cheapest agent after
// a turn: does the just-finished task look like it lacked a curated tool?
func (m model) detectImproveCmd(task, result string) tea.Cmd {
	if m.cfg.AutoImproveOff || strings.TrimSpace(result) == "" {
		return nil
	}
	a := m.cfg.Orchestrator // cheapest agent, preferring free/local
	for _, e := range m.cfg.Executors {
		if pricing.Score(e) < pricing.Score(a) {
			a = e
		}
	}
	prov := provider.For(a.Provider, a.Endpoint, a.APIKey, 256)
	model, cfg := a.Model, m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cap, ok := capabilities.Detect(ctx, prov, model, task, result, cfg)
		return improveSuggestMsg{cap: cap, ok: ok}
	}
}

// installCapability adds a curated MCP capability to the config; it activates on
// the next launch (mid-session MCP start is a later refinement).
func (m model) installCapability(c capabilities.Capability) (tea.Model, tea.Cmd) {
	capabilities.Install(&m.cfg, c)
	_ = config.Save(m.cfg)
	m.pendingCap = capabilities.Capability{}
	note := "✓ added capability: " + c.Name + " — restart cheep to activate it"
	if len(c.NeedsEnv) > 0 {
		note += " (set " + strings.Join(c.NeedsEnv, ", ") + " first)"
	}
	m.footer = note
	return m, nil
}

// pluginInstalledMsg reports the result of an on-demand plugin install.
type pluginInstalledMsg struct {
	name string
	err  error
}

// pluginInstallCmd downloads a plugin's companion binary off the UI goroutine.
func pluginInstallCmd(p plugins.Plugin) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		return pluginInstalledMsg{name: p.Name, err: p.Install(ctx)}
	}
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
			case a.Provider != "anthropic":
				// Probe the models listing — instant and queue-free. A tiny
				// completion would wait behind real inference on serial local
				// servers (LM Studio) and time out while the model is busy
				// answering the user, reporting a working agent unreachable.
				if _, _, err := provider.DiscoverModels(a.Endpoint, a.APIKey); err != nil {
					st = "unreachable"
				}
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

// bashCmd runs a shell command and returns the result as a cmdResultMsg. The
// caller's ctx makes the command interruptible (esc cancels it); a 30s timeout
// is layered on top. On cancel we kill the whole process group so children of
// `sh -c` die too, not just the shell.
func bashCmd(ctx context.Context, cmdStr string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		killProcessGroup(c) // on cancel, kill children of `sh -c` too (Unix)
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
	m.syncInputHeight()
	m.input.CursorEnd()
}

// maxInputRows caps how tall the input box grows before its inner viewport
// starts scrolling to follow the cursor.
const maxInputRows = 6

// growForNewline enlarges the input box by one row (up to maxInputRows) BEFORE
// a line break is inserted. bubbles/textarea scrolls its inner viewport to the
// cursor while the break is applied, but never scrolls back once the box grows
// afterward — so growing the box after the fact leaves the earlier lines
// scrolled out of view until the next cursor move (which is why pressing ↑
// "fixed" it). Growing first keeps the cursor inside the visible window, so no
// scroll happens and every line stays visible. syncInputHeight then sets the
// exact final height.
func (m *model) growForNewline() {
	if h := m.input.Height(); h < maxInputRows {
		m.input.SetHeight(h + 1)
	}
}

// syncInputHeight grows and shrinks the input box with its content (1–6
// rows) and re-derives the viewport height. Called on every input change so
// earlier lines never scroll out of view. Rows are counted VISUALLY: a long
// line soft-wraps, and with a too-short box the textarea's inner viewport
// chases the cursor sideways — which reads as "scrolling right, not
// wrapping".
func (m *model) syncInputHeight() {
	w := m.input.Width()
	lines := 0
	for _, l := range strings.Split(m.input.Value(), "\n") {
		if w > 0 {
			lines += lipgloss.Width(l)/w + 1 // wrapped rows, incl. the cursor's
		} else {
			lines++
		}
	}
	if lines < 1 {
		lines = 1
	}
	if lines > maxInputRows {
		lines = maxInputRows
	}
	m.input.SetHeight(lines)
	if len(m.tabs) == 0 {
		return
	}
	// Size the log to fill whatever the fixed chrome doesn't. Count the rows
	// belowViewport() will draw WITHOUT rendering them — the textarea shares an
	// internal *viewport across struct copies, so calling m.input.View() here
	// (a measurement pass) would scroll the real input box.
	const above = 2 // tab-bar banner + its separator rule
	below := 1      // the status/hint line at the very bottom
	below += len(todoHeaderLines(m.tabs[m.active].todos))
	if m.active != 0 {
		below++ // the read-only executor notice
	} else {
		if m.running && len(m.queue) > 0 {
			below += len(queueHeaderLines(m.queue))
		}
		// Next-step suggestions render as ghost text inside the input box (no
		// extra rows), so nothing to budget here.
		if len(m.comp.opts) > 0 {
			below += len(m.comp.opts) + 1 // options + the tab/enter hint row
		}
		below += 2 + lines // top rule + input box + bottom rule
	}
	m.vp.Height = max(3, m.h-above-below)
}

// belowViewport returns the chrome rendered under the scrolling log: sticky
// headers (todos, queue, suggestions, completions) and, on the orchestrator
// tab, the input box. Executor tabs are read-only — they're driven by the
// orchestrator, not typed at — so the input is replaced by a one-line notice.
// View and syncInputHeight share this so the log height always matches what's
// actually drawn.
func (m model) belowViewport() []string {
	rule := hintSt.Render(strings.Repeat("─", max(1, m.w)))
	var parts []string
	if todos := todoHeaderLines(m.tabs[m.active].todos); len(todos) > 0 {
		parts = append(parts, strings.Join(todos, "\n"))
	}
	if m.active != 0 {
		return append(parts, hintSt.Render("▲ "+m.tabs[m.active].title+
			" is driven by the orchestrator — read-only · press tab to return to the chat"))
	}
	if m.running && len(m.queue) > 0 {
		if ql := queueHeaderLines(m.queue); len(ql) > 0 {
			parts = append(parts, strings.Join(ql, "\n"))
		}
	}
	if comp := m.viewCompletions(); comp != "" {
		parts = append(parts, comp)
	}
	return append(parts, rule, m.input.View(), rule)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	nm, cmd := m.update(msg)
	mm, ok := nm.(model)
	if !ok || !mm.inline || len(mm.pendingOut) == 0 {
		return nm, cmd
	}
	prints := make([]tea.Cmd, len(mm.pendingOut))
	for i, l := range mm.pendingOut {
		l := l
		prints[i] = tea.Println(l)
	}
	mm.pendingOut = nil
	return mm, tea.Batch(tea.Sequence(prints...), cmd)
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = msg.Width
		m.input.SetWidth(msg.Width - 2)
		m.ovVP.Width = max(10, msg.Width-6)
		m.ovVP.Height = max(3, msg.Height-7)
		m.ovInput.Width = max(10, msg.Width-10)
		m.ready = true
		// Build the banner at the now-known width so no line exceeds it. Inline
		// mode prints it to the scrollback exactly once (here, not in Init);
		// the full-screen UI just refreshes it in place while it's untouched.
		if m.inline {
			if !m.welcomeShown {
				wl := welcomeLines(m.cfg, m.connectivity, m.w)
				m.tabs[0].lines = wl
				m.welcomeLen = len(wl)
				m.pendingOut = append(m.pendingOut, wl...)
				m.welcomeShown = true
			}
		} else if len(m.tabs) > 0 && len(m.tabs[0].lines) == m.welcomeLen {
			m.tabs[0].lines = welcomeLines(m.cfg, m.connectivity, m.w)
			m.welcomeLen = len(m.tabs[0].lines)
		}
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
		if m.stowing {
			// /stow finished: its report becomes a durable run note; lessons
			// were recorded by the agent via record_lesson during the turn.
			m.stowing = false
			if msg.r.Status == "completed" && strings.TrimSpace(msg.r.Output) != "" {
				history.AppendRunNote(m.workdir, "**Stowed session "+m.histID+"**\n\n"+msg.r.Output)
			}
			(&m).saveHistory()
			m.tabs[0].status = statusKey(msg.r.Status)
			m.footer = "stowed — note in ~/.cheep/history/notes.md · /clear for a fresh start"
			(&m).syncViewport()
			return m, tea.SetWindowTitle("cheep ✓ stowed — " + filepath.Base(m.workdir))
		}
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
		if m.session != nil {
			m.ctxTokens = agent.EstTokens(m.session.History())
		}
		// A completed turn is proof of life — correct a stale "unreachable"
		// from a probe that raced real inference on a serial local server.
		if msg.r.Status == "completed" && m.connectivity != nil && m.connectivity["orchestrator"] != "ok" && m.connectivity["orchestrator"] != "" {
			m.connectivity["orchestrator"] = "ok"
			if m.inline {
				m.pendingOut = append(m.pendingOut, hintSt.Render("agents: ")+okSt.Render("✓")+" orchestrator"+hintSt.Render(" (confirmed by this reply)"))
			}
		}
		// The orchestrator may have rewired cheep via the config tools. Verify the
		// new orchestrator is reachable BEFORE switching to it (don't brick the
		// session); applied in the switchMsg handler.
		if fresh, err := config.Load(); err == nil && !configEqual(fresh, m.cfg) && !configEqual(fresh, m.pendingCfg) {
			m.pendingCfg = fresh
			m.footer = "verifying new orchestrator " + fresh.Orchestrator.Model + " …"
			return m, verifyConfigCmd(fresh)
		}
		// Backstop: the orchestrator stopped short with open todos. Nudge it to
		// keep going — whether or not it delegated this turn (it may have done
		// one phase and quit with more pending, or just narrated a plan without
		// acting on it). Bounded per user message so a model that keeps stopping
		// short can't run away.
		if m.mode == orchestrator.ModeAuto && m.openTodos > 0 && len(m.cfg.Executors) > 0 &&
			msg.r.Status == "completed" && m.nudges < 3 {
			m.nudges++
			nudge := "There are still unfinished todos. Keep going: delegate the next open " +
				"item(s) to your executors and verify the results. Don't stop until every todo " +
				"is done or you hit a real blocker you must tell me about — act, don't just describe."
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
		cmds := []tea.Cmd{tea.SetWindowTitle("cheep " + mark + " " + msg.r.Status + " — " + filepath.Base(m.workdir))}
		// If the reply didn't carry inline [[NEXT]] suggestions (many models
		// ignore that), generate them async on the cheapest executor.
		if msg.r.Status == "completed" && !m.cfg.SuggestOff && len(m.suggestions) == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			m.suggestCancel = cancel
			if sc := m.suggestCmd(ctx, msg.r.Output); sc != nil {
				cmds = append(cmds, sc)
			}
		}
		// auto-improve: check whether this turn struggled for lack of a tool.
		if !m.cfg.AutoImproveOff && m.pendingCap.Name == "" {
			if dc := m.detectImproveCmd(m.lastTask, msg.r.Output); dc != nil {
				cmds = append(cmds, dc)
			}
		}
		return m, tea.Batch(cmds...)

	case suggestionsMsg:
		// Async next-step suggestions arrived; show them only if we're idle and
		// nothing (inline sentinel or a newer turn) already set/cleared them.
		if !m.running && len(m.suggestions) == 0 && len(msg.sug) > 0 {
			m.suggestions = msg.sug
			(&m).refreshPlaceholder()
			(&m).syncViewport()
		}
		return m, nil

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
		if m.inline {
			var parts []string
			for label, st := range m.connectivity {
				mark := okSt.Render("✓")
				if st != "ok" {
					mark = errSt.Render("✗")
				}
				parts = append(parts, mark+" "+label+hintSt.Render(" ("+st+")"))
			}
			sort.Strings(parts)
			if len(parts) > 0 {
				m.pendingOut = append(m.pendingOut, hintSt.Render("agents: ")+strings.Join(parts, hintSt.Render("  ·  ")))
			}
			return m, nil
		}
		if len(m.tabs) > 0 && len(m.tabs[0].lines) == m.welcomeLen { // banner untouched
			m.tabs[0].lines = welcomeLines(m.cfg, m.connectivity, m.w)
			m.welcomeLen = len(m.tabs[0].lines)
			(&m).syncViewport()
		}
		return m, nil

	case cmdResultMsg:
		m.running = false
		m.cancel = nil
		m.quitArmed = false
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
			for _, l := range welcomeLines(msg.cfg, m.connectivity, m.w) { // re-show banner with new models
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

	case improveSuggestMsg:
		if !msg.ok || m.running || capabilities.Installed(m.cfg, msg.cap.Name) {
			return m, nil
		}
		if m.cfg.AutoImproveSilent {
			return m.installCapability(msg.cap)
		}
		m.pendingCap = msg.cap
		note := "🔧 auto-improve: " + msg.cap.Name + " would help here — /autoimprove install to add it"
		if len(msg.cap.NeedsEnv) > 0 {
			note += " (needs " + strings.Join(msg.cap.NeedsEnv, ", ") + ")"
		}
		m.footer = note
		return m, nil

	case pluginInstalledMsg:
		if msg.err != nil {
			m.footer = "install failed: " + errText(msg.err)
			return m, nil
		}
		plugins.SetEnabled(&m.cfg, msg.name, true) // installed → enable by default
		_ = config.Save(m.cfg)
		m.footer = "✓ installed & enabled " + msg.name + " — restart cheep to activate it"
		return m, nil

	case updateAvailableMsg:
		m.updateVer = msg.latest
		m.footer = "cheep " + msg.latest + " is available — /upgrade to install"
		return m, nil

	case upgradeDoneMsg:
		switch {
		case msg.err != nil:
			m.footer = "upgrade failed: " + errText(msg.err)
		case msg.already:
			m.updateVer = ""
			m.footer = "already on the latest (" + msg.to + ")"
		default:
			m.updateVer = ""
			via := ""
			if msg.via == "brew" {
				via = " via Homebrew"
			}
			note := "✓ upgraded " + msg.from + " → " + msg.to + via + " — restart cheep to run the new version"
			(&m).appendLine(0, hintSt.Render(note))
			m.footer = note
			(&m).syncViewport()
		}
		return m, nil

	case tea.KeyMsg:
		sincePrevKey := time.Since(m.lastKeyAt)
		m.lastKeyAt = time.Now()
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
		// Executor tabs are read-only — there's no input box to type into (they're
		// driven by the orchestrator). Route scroll keys to the log, let global
		// navigation fall through, and swallow anything that would edit/submit.
		if m.active != 0 {
			switch msg.String() {
			case "up", "down", "k", "j", "pgup", "pgdown", "ctrl+u", "ctrl+d", "u", "d", "b", "f", " ":
				m.vp, _ = m.vp.Update(msg)
				m.follow = m.vp.AtBottom()
				return m, nil
			case "ctrl+c", "esc", "ctrl+w", "shift+tab",
				"tab", "alt+tab", "ctrl+right", "alt+shift+tab", "ctrl+left":
				// fall through to the shared handlers below
			default:
				return m, nil // ignore typing on a read-only executor tab
			}
		}
		switch msg.String() {
		case "ctrl+c":
			// With a draft in the box, Ctrl+C clears it (like Claude Code)
			// instead of quitting. Also drops out of history browsing.
			if m.active == 0 && m.input.Value() != "" {
				m.input.Reset()
				m.histIdx = len(m.inputHist)
				m.histDraft = ""
				m.quitArmed = false
				m.footer = ""
				(&m).relayout()
				return m, nil
			}
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
		case "tab", "alt+tab", "ctrl+right":
			// Plain Tab accepts the ghost suggestion when one is showing (idle,
			// empty box) — like accepting an autosuggestion. Otherwise Tab moves
			// to the next agent. alt+tab (Option+Tab) is the mac-friendly binding;
			// ctrl+right kept for other platforms (it collides with macOS
			// Mission Control, so Option+Tab is preferred there).
			if msg.String() == "tab" && m.active == 0 && m.input.Value() == "" && len(m.suggestions) > 0 {
				m.input.SetValue(m.suggestions[0])
				m.input.CursorEnd()
				m.suggestions = nil
				(&m).refreshPlaceholder()
				(&m).syncInputHeight()
				return m, nil
			}
			if len(m.tabs) > 0 {
				m.active = (m.active + 1) % len(m.tabs)
				m.follow = true
				(&m).syncViewport()
			}
			return m, nil
		case "alt+shift+tab", "ctrl+left":
			// Previous agent.
			if len(m.tabs) > 0 {
				m.active = (m.active - 1 + len(m.tabs)) % len(m.tabs)
				m.follow = true
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
		case "shift+enter", "alt+enter", "ctrl+j":
			// Insert a line break. Grow the box first so the textarea's inner
			// viewport doesn't scroll the earlier lines out of view (see
			// growForNewline). shift+enter/ctrl+enter/ctrl+j arrive here as
			// alt+enter after the keyboard translation layer.
			(&m).growForNewline()
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			(&m).syncInputHeight()
			(&m).updateCompletions()
			return m, cmd
		case "enter":
			// Terminals without bracketed paste replay a paste as keystrokes:
			// every newline arrives as a real Enter, which would submit each
			// line. Key events that close together can only be a paste —
			// insert the newline instead (like Claude Code).
			if sincePrevKey < 10*time.Millisecond {
				(&m).growForNewline()
				m.input.InsertString("\n")
				(&m).syncInputHeight()
				return m, nil
			}
			return m.submit()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		// syncInputHeight sizes the box without moving the cursor. Do NOT call
		// relayout here — its CursorEnd would snap the cursor to the end on
		// every keystroke, breaking left/right arrow navigation.
		(&m).syncInputHeight()
		(&m).updateCompletions()
		return m, cmd

	case tea.MouseMsg:
		if m.overlay != "" {
			m.ovVP, _ = m.ovVP.Update(msg)
			return m, nil
		}
		// Wheel scrolls the conversation (and cancels any in-progress selection).
		if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
			m.selecting = false
			m.vp, _ = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
			return m, nil
		}
		// In-app click-drag text selection over the conversation viewport, so you
		// can select+copy with a plain drag even though the app captures the mouse
		// (full-screen only; inline mode uses the terminal's native selection).
		if !m.inline {
			const vpTop = 2 // tab bar + separator rule sit above the viewport
			row := msg.Y - vpTop
			inVP := row >= 0 && row < m.vp.Height
			switch msg.Action {
			case tea.MouseActionPress:
				if msg.Button == tea.MouseButtonLeft && inVP {
					m.selecting = true
					m.selY0, m.selY1 = row, row
				}
			case tea.MouseActionMotion:
				if m.selecting {
					m.selY1 = max(0, min(row, m.vp.Height-1))
				}
			case tea.MouseActionRelease:
				if m.selecting {
					m.selecting = false
					if txt := m.selectedText(); strings.TrimSpace(txt) != "" {
						if err := clipboardCopy(txt); err == nil {
							n := strings.Count(txt, "\n") + 1
							unit := "lines"
							if n == 1 {
								unit = "line"
							}
							m.footer = fmt.Sprintf("✓ copied %d %s", n, unit)
						} else {
							m.footer = "copy failed: " + err.Error()
						}
					}
				}
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	(&m).syncInputHeight()
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

// plugins handles /plugins: list, install, enable, disable, remove. Install
// downloads the companion binary on demand; enable/disable persist to config.
func (m model) plugins(f []string) (tea.Model, tea.Cmd) {
	if len(f) < 2 { // bare /plugins — list catalog + state
		m.appendLine(m.active, hintSt.Render("plugins — optional companion capabilities (install-on-demand):"))
		for _, p := range plugins.Registry {
			state := hintSt.Render("not installed")
			switch {
			case plugins.Active(m.cfg, p):
				state = okSt.Render("● installed · enabled")
			case p.Installed():
				state = hintSt.Render("installed · disabled")
			}
			m.appendLine(m.active, "  "+userSt.Render(p.Name)+"  "+state)
			m.appendLine(m.active, hintSt.Render("     "+p.Summary+" → unlocks "+p.Unlocks))
		}
		m.appendLine(m.active, hintSt.Render("/plugins install <name> · enable <name> · disable <name> · remove <name>"))
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	sub := f[1]
	if len(f) < 3 {
		m.footer = "usage: /plugins " + sub + " <name>"
		return m, nil
	}
	p, ok := plugins.Find(f[2])
	if !ok {
		m.footer = "unknown plugin: " + f[2]
		return m, nil
	}
	switch sub {
	case "install", "add":
		if p.Installed() {
			m.footer = p.Name + " is already installed"
			return m, nil
		}
		m.footer = "installing " + p.Name + "…"
		return m, pluginInstallCmd(p)
	case "enable", "on":
		if !p.Installed() {
			m.footer = p.Name + " isn't installed — /plugins install " + p.Name
			return m, nil
		}
		plugins.SetEnabled(&m.cfg, p.Name, true)
		_ = config.Save(m.cfg)
		m.footer = p.Name + " enabled — restart cheep to activate"
	case "disable", "off":
		plugins.SetEnabled(&m.cfg, p.Name, false)
		_ = config.Save(m.cfg)
		m.footer = p.Name + " disabled — restart cheep to deactivate"
	case "remove", "uninstall", "rm":
		if err := p.Remove(); err != nil {
			m.footer = "remove failed: " + errText(err)
			return m, nil
		}
		plugins.SetEnabled(&m.cfg, p.Name, false)
		_ = config.Save(m.cfg)
		m.footer = p.Name + " removed"
	default:
		m.footer = "usage: /plugins install|enable|disable|remove <name>"
	}
	return m, nil
}

// autoImprove handles /autoimprove: toggle on/off, silent-install, or install
// the pending suggestion.
func (m model) autoImprove(f []string) (tea.Model, tea.Cmd) {
	if len(f) < 2 {
		state := "ON"
		if m.cfg.AutoImproveOff {
			state = "OFF"
		}
		if m.pendingCap.Name != "" {
			m.footer = "pending: " + m.pendingCap.Name + " — /autoimprove install to add it"
			return m, nil
		}
		m.footer = "auto-improve is " + state + " — /autoimprove on|off · silent on|off · install"
		return m, nil
	}
	switch f[1] {
	case "on":
		m.cfg.AutoImproveOff = false
		_ = config.Save(m.cfg)
		m.footer = "auto-improve ON — I'll suggest tools when an agent struggles"
	case "off":
		m.cfg.AutoImproveOff = true
		_ = config.Save(m.cfg)
		m.footer = "auto-improve OFF"
	case "silent":
		on := len(f) > 2 && (f[2] == "on" || f[2] == "true" || f[2] == "yes")
		m.cfg.AutoImproveSilent = on
		_ = config.Save(m.cfg)
		if on {
			m.footer = "auto-improve will now install suggestions automatically (curated catalog only)"
		} else {
			m.footer = "auto-improve will ask before installing"
		}
	case "install":
		if m.pendingCap.Name == "" {
			m.footer = "nothing pending to install"
			return m, nil
		}
		return m.installCapability(m.pendingCap)
	default:
		m.footer = "usage: /autoimprove on|off | silent on|off | install"
	}
	return m, nil
}

func (m model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	// Record for ↑/↓ recall this session (skip consecutive duplicates) and reset
	// the cursor to "new".
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
		(&m).appendUserLine(hintSt.Render("🔧 " + cmdStr))
		m.running = true
		m.tabs[0].status = "run"
		m.active = 0
		m.follow = true
		m.started = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		m.cancel = cancel
		return m, tea.Batch(m.sp.Tick, bashCmd(ctx, cmdStr))
	}
	return m.sendUser(text, userLine(text, m.w))
}

// appendUserLine adds your message to the log with a blank line on each side,
// so there's clear breathing room between the previous reply, your message, and
// the agent's response — no separator rules.
func (m *model) appendUserLine(display string) {
	m.appendLine(0, "")
	m.appendLine(0, display)
	m.appendLine(0, "")
}

// sendUser routes a user message to the orchestrator with the usual guards
// (no session, task already running, over budget). display is the line shown
// in the log — for prompt templates it is the typed /name, not the expansion.
func (m model) sendUser(text, display string) (tea.Model, tea.Cmd) {
	if m.session == nil {
		// Show the message and a clear, actionable error in the conversation.
		(&m).appendUserLine(display)
		(&m).appendLine(0, errSt.Render("✗ can't run")+hintSt.Render(" — "+errText(m.buildErr)+"  ·  fix with /config or /setup"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	if m.running {
		// Queue it — it runs automatically when the current task finishes.
		m.queue = append(m.queue, text)
		(&m).appendUserLine(display + "  " + hintSt.Render("(queued)"))
		m.active = 0
		m.follow = true
		(&m).syncViewport()
		return m, nil
	}
	if m.overBudget() {
		(&m).appendUserLine(display)
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
	return m.runMessage(text, userLine(text, m.w))
}

// runMessage sends text to the orchestrator, showing `display` in the log.
func (m model) runMessage(text, display string) (tea.Model, tea.Cmd) {
	m.lastTask = text   // for auto-improve gap detection at turn-end
	m.suggestions = nil // last turn's suggestions are stale once we act
	m.refreshPlaceholder()
	if m.suggestCancel != nil {
		m.suggestCancel() // free the endpoint from any in-flight suggestion call
		m.suggestCancel = nil
	}
	(&m).appendUserLine(display)
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

// stowPrompt sweeps session knowledge to disk before a reset (firstmate's
// /stow): durable lessons via record_lesson, plus a structured handoff report
// that cheep appends to the run notes.
const stowPrompt = `Stow this session before a context reset. Do these now:
1. For each durable lesson learned this session about how THIS project works (a convention,
   a command, a gotcha the user corrected), call record_lesson once with one concise
   sentence. Skip trivia. If record_lesson is unavailable, fold the lessons into the report.
2. Then reply with a stow report in EXACTLY this structure:
## Done
- what was accomplished and verified
## In flight
- anything unfinished and where it stands (branches, failing checks, open questions)
## Next steps
- the concrete actions a fresh session should take first
Keep it terse and factual — it is the handoff for a session with no memory of this one.`

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
	case "/mouse":
		if m.inline {
			m.footer = "inline mode: native scrolling & selection are always on — /mouse only applies to the full-screen UI"
			return m, nil
		}
		m.mouseOn = !m.mouseOn
		m.cfg.MouseOff = !m.mouseOn
		_ = config.Save(m.cfg) // sticky across sessions
		if m.mouseOn {
			m.footer = "mouse capture ON (saved) — wheel scrolls the conversation; hold Option (macOS) or Shift to select & copy text"
			return m, tea.EnableMouseCellMotion
		}
		m.footer = "mouse capture OFF (saved) — select/copy text natively; scroll with the wheel disabled, use pgup/pgdn (or /copy)"
		// Re-disable alt-scroll so the wheel can't spray ↑/↓ (history recall) now
		// that we're no longer capturing it as mouse events.
		return m, tea.Sequence(tea.DisableMouse, disableAltScroll)
	case "/copy":
		if m.session == nil {
			m.footer = "nothing to copy yet"
			return m, nil
		}
		text := ""
		for _, msg := range m.session.History() {
			if msg.Role == "assistant" && strings.TrimSpace(msg.Text) != "" {
				text = msg.Text
			}
		}
		if text == "" {
			m.footer = "no reply to copy yet"
			return m, nil
		}
		if err := clipboardCopy(text); err != nil {
			m.footer = "copy failed: " + err.Error()
		} else {
			m.footer = fmt.Sprintf("copied the last reply (%d chars) to the clipboard", len(text))
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
	case "/queue", "/dequeue", "/unqueue":
		return m.queueCmd(text)
	case "/clear":
		(&m).saveHistory() // keep the conversation we're clearing
		m.tabs = []*tab{{id: "orchestrator", title: "orchestrator", lines: welcomeLines(m.cfg, m.connectivity, m.w)}}
		m.welcomeLen = len(m.tabs[0].lines)
		m.byName = map[string]int{"orchestrator": 0, "cheep": 0}
		m.active = 0
		(&m).rebuild(false)
		// start a fresh history session (a new root, not a fork)
		m.histStarted = time.Now()
		m.histID = history.UniqueID(m.histStarted)
		m.histTitle = ""
		m.histParent, m.histForkAt = "", 0
		m.ctxTokens = 0
		if m.inline {
			m.pendingOut = append(m.pendingOut, "", hintSt.Render("── cleared · fresh session "+m.histID+" ──"), "")
		}
		m.footer = "(cleared)"
		(&m).syncViewport()
	case "/history", "/resume":
		return m.openHistory()
	case "/fork":
		return m.openFork()
	case "/tree":
		return m.openTree()
	case "/suggest", "/suggestions":
		m.cfg.SuggestOff = !m.cfg.SuggestOff
		_ = config.Save(m.cfg)
		(&m).rebuild(true)
		if m.cfg.SuggestOff {
			m.suggestions = nil
			(&m).refreshPlaceholder()
			m.footer = "next-step suggestions OFF"
		} else {
			m.footer = "next-step suggestions ON — after a reply, Tab accepts the suggested next step"
		}
	case "/scheduled", "/schedule":
		return m.openScheduled()
	case "/cd":
		f := strings.Fields(text)
		if len(f) < 2 {
			m.footer = "workspace: " + m.workdir + " — /cd <path> moves the agents to another directory"
			return m, nil
		}
		target := strings.TrimSpace(strings.TrimPrefix(text, "/cd "))
		if strings.HasPrefix(target, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				target = home + strings.TrimPrefix(target, "~")
			}
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(m.workdir, target)
		}
		target = filepath.Clean(target)
		info, err := os.Stat(target)
		if err != nil || !info.IsDir() {
			m.footer = "can't cd there — no such directory: " + target
			return m, nil
		}
		m.workdir = target
		m.fileList = loadFileList(target)
		(&m).rebuild(true) // re-scope tools, worktree pool, project instructions; keep the conversation
		if m.inline {
			m.pendingOut = append(m.pendingOut, hintSt.Render("⟳ workspace → "+target))
		}
		m.footer = "workspace → " + target
		return m, tea.SetWindowTitle("cheep — " + filepath.Base(target))
	case "/stow":
		if m.session == nil || len(m.session.History()) == 0 {
			m.footer = "nothing to stow yet"
			return m, nil
		}
		if m.running {
			m.footer = "a task is running — stow when it finishes"
			return m, nil
		}
		m.stowing = true
		return m.sendUser(stowPrompt, hintSt.Render("⚓ stowing session knowledge to disk"))
	case "/nomistakes", "/no-mistakes":
		f := strings.Fields(text)
		if len(f) < 2 {
			state := "OFF"
			if m.cfg.NoMistakes {
				state = "ON"
			}
			m.footer = "no-mistakes: " + state + " — /nomistakes on|off (every write/command asks; merges need your sign-off)"
			return m, nil
		}
		switch f[1] {
		case "on":
			m.cfg.NoMistakes = true
			m.gate.SetMode(approve.ModeApprove)
			_ = config.Save(m.cfg)
			(&m).rebuild(true)
			m.footer = "no-mistakes ON — shared writes/commands ask first, and nothing merges without your approval"
		case "off":
			m.cfg.NoMistakes = false
			md, ok := approve.ParseMode(m.cfg.ApprovalMode)
			if !ok {
				md = approve.ModeAuto
			}
			m.gate.SetMode(md)
			_ = config.Save(m.cfg)
			(&m).rebuild(true)
			m.footer = "no-mistakes OFF — approvals back to " + string(md) + ", merges are automatic again"
		default:
			m.footer = "usage: /nomistakes on|off"
		}
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
	case "/upgrade", "/update":
		m.footer = "checking for updates…"
		return m, upgradeCmd()
	case "/plugins", "/plugin":
		return m.plugins(strings.Fields(text))
	case "/autoimprove", "/improve":
		return m.autoImprove(strings.Fields(text))
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
		if m.ctxTokens > 0 && m.cfg.Orchestrator.ContextBudget > 0 {
			m.appendLine(m.active, hintSt.Render(fmt.Sprintf(
				"  context: ~%s of %s est. tokens — auto-compacts at 100%%, squeezed-out memory saved to ~/.cheep/history/notes.md",
				human(m.ctxTokens), human(m.cfg.Orchestrator.ContextBudget))))
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
			return m.sendUser(prompts.Expand(t, args), userLine(text, m.w))
		}
		m.footer = "unknown command " + strings.Fields(text)[0]
	}
	return m, nil
}

// queueCmd lists and removes messages waiting to run. /queue lists them,
// /queue clear empties the queue, and /queue rm N (or /dequeue N, /unqueue N,
// or a bare /queue N) drops one by its 1-based position — the same numbering
// shown in the "Queued Tasks" panel.
func (m model) queueCmd(text string) (tea.Model, tea.Cmd) {
	f := strings.Fields(text)
	cmd := strings.TrimPrefix(f[0], "/")
	if len(m.queue) == 0 {
		m.footer = "the queue is empty — messages you type while a task runs land here"
		return m, nil
	}
	arg := ""
	if len(f) > 1 {
		arg = f[1]
	}
	if cmd == "queue" {
		switch arg {
		case "": // list
			m.appendLine(0, hintSt.Render("queued messages — remove with /queue rm N, or /queue clear:"))
			for i, q := range m.queue {
				m.appendLine(0, "  "+hintSt.Render(fmt.Sprintf("%d. ", i+1))+short(q, 100))
			}
			m.active, m.follow = 0, true
			(&m).syncViewport()
			return m, nil
		case "clear", "empty", "flush":
			n := len(m.queue)
			m.queue = nil
			m.footer = fmt.Sprintf("cleared %d queued message(s)", n)
			(&m).syncViewport()
			return m, nil
		case "rm", "remove", "del", "delete":
			arg = "" // the index is the next field
			if len(f) > 2 {
				arg = f[2]
			}
		}
	}
	idx, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || idx < 1 || idx > len(m.queue) {
		m.footer = fmt.Sprintf("usage: /queue (list) · /queue rm <1-%d> · /queue clear", len(m.queue))
		return m, nil
	}
	removed := m.queue[idx-1]
	m.queue = append(m.queue[:idx-1:idx-1], m.queue[idx:]...)
	m.footer = "removed from queue: " + short(removed, 60)
	(&m).syncViewport()
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
	if m.inline {
		if idx != 0 { // executor output is prefixed instead of tabbed
			line = hintSt.Render("⟨"+m.tabs[idx].title+"⟩ ") + line
		}
		m.pendingOut = append(m.pendingOut, line)
	}
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
	case "progress":
		if idx == 0 && e.Ctx > 0 {
			m.ctxTokens = e.Ctx
		}
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
		txt := e.Text
		if idx == 0 { // pull "next step" suggestions off the orchestrator's reply
			if clean, sug := splitSuggestions(txt); len(sug) > 0 {
				txt, m.suggestions = clean, sug
				m.refreshPlaceholder()
			}
		}
		if strings.TrimSpace(txt) != "" {
			for _, l := range renderMarkdown(txt, m.w) {
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
		if m.updateVer != "" {
			// Persistent badge (survives footer changes) once the launch check
			// finds a newer release.
			status = hintSt.Render("↑ "+m.updateVer+" available · /upgrade") + "   " + status
		}
		if m.footer != "" {
			status = m.footer + "   " + status
		}
	}
	left := modeLabel(m.mode) + "   " + status
	hint := left
	right := hintSt.Render(m.tokenSummary())
	if cb := m.ctxBar(); cb != "" {
		if right != "" {
			right = cb + hintSt.Render("  ·  ") + right
		} else {
			right = cb
		}
	}
	if tok := right; tok != "" {
		gap := m.w - lipgloss.Width(left) - lipgloss.Width(tok)
		if gap < 1 {
			gap = 1
		}
		hint = left + strings.Repeat(" ", gap) + tok
	}
	rule := hintSt.Render(strings.Repeat("─", max(1, m.w)))
	if m.inline {
		// Inline chrome only: the conversation itself lives in the terminal
		// scrollback (via tea.Println), like Claude Code. The agent banner is
		// pinned here in the bottom chrome (there's no top tab bar in inline
		// mode), so it stays visible while the scrollback scrolls natively.
		parts := []string{m.agentBar()}
		if todos := todoHeaderLines(m.tabs[m.active].todos); len(todos) > 0 {
			parts = append(parts, strings.Join(todos, "\n"))
		}
		if m.running && len(m.queue) > 0 {
			if ql := queueHeaderLines(m.queue); len(ql) > 0 {
				parts = append(parts, strings.Join(ql, "\n"))
			}
		}
		if comp := m.viewCompletions(); comp != "" {
			parts = append(parts, comp)
		}
		parts = append(parts, rule, m.input.View(), hint)
		return lipgloss.JoinVertical(lipgloss.Left, parts...)
	}
	// The tab bar is a fixed banner pinned to the top; the log scrolls beneath
	// it and never covers it. belowViewport supplies the sticky panels and the
	// input box (hidden on read-only executor tabs).
	parts := []string{m.tabBar(), m.bannerSep(), m.highlightSelection(m.vp.View())}
	parts = append(parts, m.belowViewport()...)
	parts = append(parts, hint)
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

// bannerSep is the rule under the tab-bar banner that separates the fixed
// agent banner from the scrolling conversation below it.
func (m model) bannerSep() string {
	return sepSt.Render(strings.Repeat("─", max(1, m.w)))
}

// selRange returns the ordered [lo, hi] viewport rows of the current selection.
func (m model) selRange() (int, int) {
	if m.selY0 <= m.selY1 {
		return m.selY0, m.selY1
	}
	return m.selY1, m.selY0
}

// selectedText returns the plain text of the current viewport selection — the
// visible lines between the drag anchor and head, ANSI stripped. Line-level for
// now (a whole visible row at a time); char-level ranges are a later refinement.
func (m model) selectedText() string {
	lines := strings.Split(m.vp.View(), "\n")
	lo, hi := m.selRange()
	var out []string
	for i := lo; i <= hi && i < len(lines); i++ {
		if i >= 0 {
			out = append(out, strings.TrimRight(ansi.Strip(lines[i]), " "))
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// highlightSelection reverse-videos the selected rows in a rendered viewport
// so the drag is visible.
func (m model) highlightSelection(vpview string) string {
	if !m.selecting {
		return vpview
	}
	lines := strings.Split(vpview, "\n")
	lo, hi := m.selRange()
	for i := lo; i <= hi && i < len(lines); i++ {
		if i >= 0 {
			lines[i] = selHiSt.Render(ansi.Strip(lines[i]))
		}
	}
	return strings.Join(lines, "\n")
}

// agentBar is the inline-mode equivalent of the top tab bar: a compact,
// always-visible banner of the active agents (orchestrator + any executors)
// with status glyphs. Pinned in the inline bottom chrome so it stays put while
// the conversation scrolls in the native scrollback above.
func (m model) agentBar() string {
	var parts []string
	for _, t := range m.tabs {
		parts = append(parts, glyph(t.status)+" "+t.title)
	}
	bar := strings.Join(parts, hintSt.Render("  ·  "))
	if m.w > 0 && lipgloss.Width(bar) > m.w {
		bar = ansi.TruncateWc(bar, m.w, "…")
	}
	return barSt.Width(m.w).Render(bar)
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
	// Once there's more than the orchestrator tab, surface how to reach the
	// executor / reviewer tabs — the hotkey isn't otherwise discoverable.
	// Only add it if it still fits on one line; see the truncation note below.
	if len(m.tabs) > 1 {
		hint := hintSt.Render("   tab / ⌥tab: switch agents · ^W: close")
		if m.w <= 0 || lipgloss.Width(bar)+lipgloss.Width(hint) <= m.w {
			bar = lipgloss.JoinHorizontal(lipgloss.Top, bar, hint)
		}
	}
	// Tabs accumulate over a long session; keep the bar to a single line no
	// matter how many pile up. relayout() budgets a fixed row for the tab
	// bar, and Style.Render's Width() word-wraps rather than truncates — an
	// overflowing bar would wrap onto a second line and push everything
	// below it (including the tabs themselves) out of the viewport.
	if m.w > 0 && lipgloss.Width(bar) > m.w {
		bar = ansi.TruncateWc(bar, m.w, "…")
	}
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
	// Ignore runtime-resolved fields: context windows are detected in memory
	// at startup (not written to disk), so a fresh config.Load() would always
	// differ from the live m.cfg and falsely read as "the agent rewired cheep"
	// — which reprinted the banner mid-session and dropped window detection.
	strip := func(c config.Config) config.Config {
		c.Orchestrator.ContextWindow, c.Orchestrator.ContextBudget, c.Orchestrator.TokenBudget = 0, 0, 0
		ex := make([]config.Agent, len(c.Executors))
		copy(ex, c.Executors)
		for i := range ex {
			ex[i].ContextWindow, ex[i].ContextBudget, ex[i].TokenBudget = 0, 0, 0
		}
		c.Executors = ex
		return c
	}
	x, _ := json.Marshal(strip(a))
	y, _ := json.Marshal(strip(b))
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

// ctxBar renders the orchestrator's context fill against its compaction
// budget: green → yellow (60%) → red (85%); compaction fires at 100%.
func (m model) ctxBar() string {
	budget := m.cfg.Orchestrator.ContextBudget
	if budget <= 0 || m.ctxTokens <= 0 {
		return ""
	}
	ratio := float64(m.ctxTokens) / float64(budget)
	if ratio > 1 {
		ratio = 1
	}
	const cells = 8
	filled := int(ratio*cells + .5)
	st := okSt
	switch {
	case ratio >= .85:
		st = errSt
	case ratio >= .6:
		st = todoProgSt
	}
	return hintSt.Render("ctx ") + st.Render(strings.Repeat("▰", filled)) +
		hintSt.Render(strings.Repeat("▱", cells-filled)) + hintSt.Render(fmt.Sprintf(" %d%%", int(ratio*100)))
}

// place centers an overlay box in the alt-screen UI; inline mode just returns
// the box (a full-height Place would stomp the scrollback area).
func (m model) place(box string) string {
	if m.inline {
		return box
	}
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
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
