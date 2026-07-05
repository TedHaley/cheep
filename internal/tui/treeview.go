package tui

// The session tree (a pi-style feature): /fork branches the current
// conversation from any earlier user turn — the prefix is kept, everything
// after it is left on the old branch — and /tree shows every saved session as
// a tree (forks nested under their parent) for navigation.

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/history"
	"github.com/TedHaley/cheep/internal/orchestrator"
)

// openFork lists the current conversation's user turns as fork points.
func (m model) openFork() (tea.Model, tea.Cmd) {
	if m.running {
		m.footer = "a task is running — esc to cancel it before forking"
		return m, nil
	}
	if m.session == nil {
		m.footer = "no session to fork"
		return m, nil
	}
	m.forkPoints = m.forkPoints[:0]
	for i, msg := range m.session.History() {
		if msg.Role == "user" && strings.TrimSpace(msg.Text) != "" {
			m.forkPoints = append(m.forkPoints, i)
		}
	}
	if len(m.forkPoints) == 0 {
		m.footer = "nothing to fork yet — send a message first"
		return m, nil
	}
	m.forkCursor = len(m.forkPoints) - 1
	m.overlay = "fork"
	return m, nil
}

func (m model) updateFork(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc", "q":
		m.overlay = ""
	case "up", "k":
		if m.forkCursor > 0 {
			m.forkCursor--
		}
	case "down", "j":
		if m.forkCursor < len(m.forkPoints)-1 {
			m.forkCursor++
		}
	case "enter":
		if len(m.forkPoints) > 0 {
			return m.doFork(m.forkPoints[m.forkCursor])
		}
	}
	return m, nil
}

// doFork starts a new session whose history is the current one up to (but not
// including) the user turn at msgIdx — you then type an alternative to it.
func (m model) doFork(msgIdx int) (tea.Model, tea.Cmd) {
	(&m).saveHistory() // the branch we're leaving keeps everything

	msgs := append([]core.Message{}, m.session.History()[:msgIdx]...)
	parent := m.histID
	m.histParent = parent
	m.histForkAt = msgIdx
	m.histStarted = time.Now()
	m.histID = history.UniqueID(m.histStarted)
	m.histTitle = ""

	orch, err := orchestrator.Build(m.cfg, m.workdir, orchestrator.Options{
		Isolate: true, Mode: m.mode, ExtraOrch: m.extraOrch, ExtraExec: m.extraExec, OnEvent: m.onEvent,
		Gate: m.gate,
	})
	m.buildErr = err
	if err != nil {
		m.session = nil
		m.overlay = ""
		m.footer = "fork failed: " + errText(err)
		return m, nil
	}
	m.session = orch.Resume(msgs)

	lines := welcomeLines(m.cfg, m.connectivity)
	lines = append(lines, hintSt.Render("⑂ forked from "+parent+" — type an alternative to the turn you replaced"), "")
	for _, msg := range msgs {
		lines = append(lines, replayLines(msg, m.w)...)
	}
	m.tabs[0].lines = lines
	m.overlay = ""
	m.active, m.follow = 0, true
	m.footer = "forked · new branch " + m.histID
	(&m).syncViewport()
	return m, nil
}

func (m model) viewFork() string {
	title := lipgloss.NewStyle().Bold(true).Render("Fork session")
	lines := []string{title, hintSt.Render("Pick the user turn to redo — history before it is kept."), ""}
	msgs := []string{}
	if m.session != nil {
		for _, i := range m.forkPoints {
			msgs = append(msgs, m.session.History()[i].Text)
		}
	}
	for i, txt := range msgs {
		cur := "  "
		if i == m.forkCursor {
			cur = todoProgSt.Render("▸ ")
		}
		lines = append(lines, cur+short(strings.ReplaceAll(txt, "\n", " "), 60))
	}
	lines = append(lines, "", hintSt.Render("↑/↓ move · enter fork · esc close"))
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
		ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}

// openTree shows the whole session tree (including the current session).
func (m model) openTree() (tea.Model, tea.Cmd) {
	(&m).saveHistory() // make sure the current session appears in the tree
	metas, depths, _ := history.Tree()
	if len(metas) == 0 {
		m.footer = "no saved sessions yet"
		return m, nil
	}
	m.histList, m.histDepth = metas, depths
	m.histCursor = 0
	for i, h := range metas {
		if h.ID == m.histID {
			m.histCursor = i
			break
		}
	}
	m.overlay = "tree"
	return m, nil
}

func (m model) updateTree(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc", "q":
		m.overlay = ""
	case "up", "k":
		if m.histCursor > 0 {
			m.histCursor--
		}
	case "down", "j":
		if m.histCursor < len(m.histList)-1 {
			m.histCursor++
		}
	case "enter":
		if len(m.histList) > 0 {
			sel := m.histList[m.histCursor]
			if sel.ID == m.histID {
				m.overlay = ""
				m.footer = "already on this session"
				return m, nil
			}
			return m.resumeHistory(sel.ID)
		}
	}
	return m, nil
}

func (m model) viewTree() string {
	title := lipgloss.NewStyle().Bold(true).Render("Session tree")
	lines := []string{title, hintSt.Render("Forks nest under the session they branched from."), ""}
	for i, h := range m.histList {
		cur := "  "
		if i == m.histCursor {
			cur = todoProgSt.Render("▸ ")
		}
		indent := strings.Repeat("  ", m.histDepth[i])
		branch := ""
		if m.histDepth[i] > 0 {
			branch = "⑂ "
		}
		t := h.Title
		if t == "" {
			t = "(untitled)"
		}
		marker := ""
		if h.ID == m.histID {
			marker = okSt.Render(" ● current")
		}
		meta := h.Updated.Local().Format("Jan 2 15:04")
		lines = append(lines, cur+indent+branch+short(t, 48)+hintSt.Render("  "+meta)+marker)
	}
	lines = append(lines, "", hintSt.Render("↑/↓ move · enter switch · esc close"))
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
		ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}
