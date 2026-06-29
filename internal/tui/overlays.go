package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/configassist"
	"github.com/TedHaley/cheep/internal/core"
)

type ovDoneMsg struct{}

// formatEvent renders one event as a log line (used by the setup overlay).
func formatEvent(e core.Event) string {
	switch e.Type {
	case "text":
		return e.Text
	case "tool_call":
		return "→ " + e.Tool + "(" + shortArgs(e.Args) + ")"
	case "tool_result":
		return "  ← " + short(e.Result, 300)
	case "status":
		return "• " + e.Status
	case "error":
		return "✗ " + e.Text
	case "lifecycle":
		if e.Status == "start" {
			return "● started"
		}
		return "■ " + e.Status
	}
	return ""
}

// openSetup launches the in-app setup assistant (chat with a reachable agent).
func (m model) openSetup() (tea.Model, tea.Cmd) {
	asst, st, label, err := configassist.Build(m.cfg, m.onEvent)
	if err != nil {
		m.footer = err.Error()
		return m, nil
	}
	ti := textinput.New()
	ti.Placeholder = "e.g. add an executor at http://localhost:1234"
	ti.Focus()
	ti.Width = max(10, m.w-10)

	m.overlay = "setup"
	m.ovTitle = "setup — powered by " + label
	m.ovState = st
	m.ovSess = asst.NewSession()
	m.ovBusy = false
	m.ovLog = []string{"Describe what to configure. Enter to send · Esc or /done to finish."}
	m.ovInput = ti
	m.ovVP = viewport.New(max(10, m.w-6), max(3, m.h-7))
	m.ovVP.SetContent(strings.Join(m.ovLog, "\n"))
	return m, textinput.Blink
}

func (m model) closeSetup() (tea.Model, tea.Cmd) {
	saved := m.ovState != nil && m.ovState.Saved
	m.overlay = ""
	m.ovSess = nil
	m.ovState = nil
	if saved {
		if c, err := config.Load(); err == nil {
			m.cfg = c
		}
		(&m).rebuild(true)
		m.footer = "configuration updated"
	}
	return m, nil
}

func (m model) updateOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.overlay == "help" {
		m.overlay = "" // any key closes
		return m, nil
	}
	if m.overlay == "setupwiz" {
		return m.updateWiz(msg)
	}
	if m.overlay == "history" {
		return m.updateHistory(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		return m.closeSetup()
	case "pgup", "pgdown", "up", "down":
		m.ovVP, _ = m.ovVP.Update(msg)
		return m, nil
	case "enter":
		text := strings.TrimSpace(m.ovInput.Value())
		m.ovInput.Reset()
		if text == "" {
			return m, nil
		}
		if text == "/done" || text == "/cancel" {
			return m.closeSetup()
		}
		if m.ovBusy {
			return m, nil
		}
		m.ovLog = append(m.ovLog, "› "+text)
		m.ovVP.SetContent(strings.Join(m.ovLog, "\n"))
		m.ovVP.GotoBottom()
		m.ovBusy = true
		s := m.ovSess
		return m, func() tea.Msg { s.Send(text); return ovDoneMsg{} }
	}
	var cmd tea.Cmd
	m.ovInput, cmd = m.ovInput.Update(msg)
	return m, cmd
}

var ovBox = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

func (m model) viewOverlay() string {
	if m.overlay == "help" {
		return m.viewHelp()
	}
	if m.overlay == "setupwiz" {
		return m.viewWiz()
	}
	if m.overlay == "history" {
		return m.viewHistory()
	}
	title := lipgloss.NewStyle().Bold(true).Render(m.ovTitle)
	m.ovInput.Prompt = "setup › "
	hint := hintSt.Render("Enter: send · Esc or /done: finish · pgup/pgdn: scroll")
	body := lipgloss.JoinVertical(lipgloss.Left, title, "", m.ovVP.View(), m.ovInput.View(), hint)
	return ovBox.Width(m.w - 2).Render(body)
}

func (m model) viewHelp() string {
	b := lipgloss.NewStyle().Bold(true)
	lines := []string{
		b.Render("cheep — help"),
		"",
		"modes:   shift+tab cycles chat → plan → auto  (or /chat /plan /auto)",
		"         multi-agent delegation happens only in AUTO mode;",
		"         chat = talk only, plan = read-only investigation.",
		"glyphs:  ● running · ✓ done · ⚠ stopped early · ✗ error",
		"agents:  tab = next · ctrl+←/→ = prev/next · pgup/pgdn = scroll",
		"         ctrl+w (or /close) closes the focused executor tab",
		"         /keeptabs toggles auto-close of finished executor tabs",
		"cancel:  esc cancels a running task",
		"",
		"commands:",
		"  /config          set up agents from discovered servers + keys",
		"  /setup           configure by chatting with a working agent",
		"  /status          show current setup",
		"  /tokens          token usage + estimated $ per model (and savings)",
		"  /budget          set a session $ cap (warns at 80%, stops at 100%)",
		"  /history /resume browse and resume past conversations",
		"  /clear           reset the conversation",
		"  /help            this panel        /exit  quit",
		"",
		hintSt.Render("press any key to close"),
	}
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
		ovBox.Padding(1, 3).Render(strings.Join(lines, "\n")))
}
