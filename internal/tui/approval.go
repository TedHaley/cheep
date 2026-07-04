package tui

// The approval overlay: gated tool calls (see internal/approve) block their
// agent goroutine until the user answers here. Requests queue FIFO; the
// overlay shows one at a time with a colored diff preview for file writes.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/approve"
	"github.com/TedHaley/cheep/internal/config"
)

// approvalMsg carries one gate request into the Bubble Tea loop.
type approvalMsg struct{ req approve.Request }

var (
	diffAddSt = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	diffDelSt = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	diffCtxSt = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func (m model) pushApproval(r approve.Request) (tea.Model, tea.Cmd) {
	m.approvals = append(m.approvals, r)
	if m.overlay == "" { // don't steal an open wizard/help/setup overlay
		m.overlay = "approval"
		m.syncApprovalVP()
	}
	return m, nil
}

// syncApprovalVP loads the current request's preview into the overlay viewport.
func (m *model) syncApprovalVP() {
	if len(m.approvals) == 0 {
		return
	}
	r := m.approvals[0]
	var body string
	switch r.Tool {
	case "write_file":
		body = colorDiff(r.Diff)
	case "run_bash":
		body = "$ " + r.Cmd
	default:
		body = fmt.Sprintf("%v", r.Args)
	}
	m.ovVP.Width = m.w - 6
	m.ovVP.Height = min(m.h-10, max(6, strings.Count(body, "\n")+1))
	m.ovVP.SetContent(body)
	m.ovVP.GotoTop()
}

// answerApproval resolves the current request and advances the queue.
func (m model) answerApproval(d approve.Decision) (tea.Model, tea.Cmd) {
	if len(m.approvals) == 0 {
		m.overlay = ""
		return m, nil
	}
	r := m.approvals[0]
	m.approvals = m.approvals[1:]
	select {
	case r.Resp <- d: // buffered(1); never blocks
	default:
	}
	if len(m.approvals) == 0 {
		m.overlay = ""
	} else {
		m.syncApprovalVP()
	}
	return m, nil
}

func (m model) updateApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		return m.answerApproval(approve.Allow)
	case "n", "esc":
		return m.answerApproval(approve.Deny)
	case "a":
		return m.answerApproval(approve.AllowSession)
	case "up", "down", "pgup", "pgdown":
		m.ovVP, _ = m.ovVP.Update(msg)
		return m, nil
	}
	return m, nil
}

func (m model) viewApproval() string {
	if len(m.approvals) == 0 {
		return ""
	}
	r := m.approvals[0]
	title := "approve: " + r.Tool
	if r.Path != "" {
		title += " → " + r.Path
	}
	head := lipgloss.NewStyle().Bold(true).Render(title)
	if n := len(m.approvals); n > 1 {
		head += hintSt.Render(fmt.Sprintf("  (+%d queued)", n-1))
	}
	hint := hintSt.Render("y/enter: allow · n/esc: deny · a: allow this tool for the session · ↑/↓: scroll")
	body := lipgloss.JoinVertical(lipgloss.Left, head, "", m.ovVP.View(), "", hint)
	return ovBox.Width(m.w - 2).Render(body)
}

// colorDiff colors approve.Diff output by line prefix.
func colorDiff(d string) string {
	lines := strings.Split(d, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+"):
			lines[i] = diffAddSt.Render(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = diffDelSt.Render(l)
		default:
			lines[i] = diffCtxSt.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// setApprovalMode applies and persists an approval mode change.
func (m model) setApprovalMode(arg string) (tea.Model, tea.Cmd) {
	if arg == "" {
		m.footer = "approvals: " + string(m.gate.Mode()) + " — /approval yolo|auto|approve to change"
		return m, nil
	}
	md, ok := approve.ParseMode(arg)
	if !ok {
		m.footer = "unknown approval mode " + arg + " (yolo|auto|approve)"
		return m, nil
	}
	m.gate.SetMode(md)
	m.cfg.ApprovalMode = string(md)
	if err := config.Save(m.cfg); err != nil {
		m.footer = "approvals: " + string(md) + " (not saved: " + err.Error() + ")"
	} else {
		m.footer = "approvals: " + string(md)
	}
	return m, nil
}
