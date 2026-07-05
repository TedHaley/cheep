package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/core"
	"github.com/TedHaley/cheep/internal/history"
	"github.com/TedHaley/cheep/internal/orchestrator"
)

// saveHistory persists the current conversation (JSON record + Markdown
// transcript) to ~/.cheep/history. Called after each completed turn.
func (m *model) saveHistory() {
	if m.session == nil {
		return
	}
	msgs := m.session.History()
	if len(msgs) == 0 {
		return
	}
	if m.histTitle == "" {
		for _, mm := range msgs {
			if mm.Role == "user" && strings.TrimSpace(mm.Text) != "" {
				m.histTitle = short(mm.Text, 60)
				break
			}
		}
	}
	_ = history.Save(history.Record{
		ID: m.histID, Parent: m.histParent, ForkAt: m.histForkAt,
		Started: m.histStarted, Updated: time.Now(),
		Workdir: m.workdir, Title: m.histTitle, Messages: msgs,
	})
}

// openHistory lists past sessions (excluding the current one) for resume.
func (m model) openHistory() (tea.Model, tea.Cmd) {
	list, _ := history.List()
	out := list[:0]
	for _, h := range list {
		if h.ID != m.histID {
			out = append(out, h)
		}
	}
	m.histList = out
	m.histCursor = 0
	m.overlay = "history"
	return m, nil
}

func (m model) updateHistory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.overlay = ""
		return m, nil
	case "esc", "q":
		m.overlay = ""
		return m, nil
	case "up", "k":
		if m.histCursor > 0 {
			m.histCursor--
		}
	case "down", "j":
		if m.histCursor < len(m.histList)-1 {
			m.histCursor++
		}
	case "d":
		if len(m.histList) > 0 {
			_ = history.Delete(m.histList[m.histCursor].ID)
			m.histList = append(m.histList[:m.histCursor], m.histList[m.histCursor+1:]...)
			if m.histCursor >= len(m.histList) && m.histCursor > 0 {
				m.histCursor--
			}
		}
	case "enter":
		if len(m.histList) > 0 {
			return m.resumeHistory(m.histList[m.histCursor].ID)
		}
	}
	return m, nil
}

// resumeHistory loads a session into the agent's context and replays it on screen.
func (m model) resumeHistory(id string) (tea.Model, tea.Cmd) {
	r, err := history.Load(id)
	if err != nil {
		m.overlay = ""
		m.footer = "couldn't load session: " + err.Error()
		return m, nil
	}
	(&m).saveHistory() // keep the session we're leaving

	m.histID, m.histStarted, m.histTitle = r.ID, r.Started, r.Title
	m.histParent, m.histForkAt = r.Parent, r.ForkAt
	orch, berr := orchestrator.Build(m.cfg, m.workdir, orchestrator.Options{
		Isolate: true, Mode: m.mode, ExtraOrch: m.extraOrch, ExtraExec: m.extraExec, OnEvent: m.onEvent,
		Gate: m.gate,
	})
	m.buildErr = berr
	if berr != nil {
		m.session = nil
	} else {
		m.session = orch.Resume(r.Messages)
	}

	lines := welcomeLines(m.cfg, m.connectivity)
	lines = append(lines, hintSt.Render("↻ resumed session "+r.ID), "")
	for _, msg := range r.Messages {
		lines = append(lines, replayLines(msg, m.w)...)
	}
	m.tabs[0].lines = lines
	m.overlay = ""
	m.active, m.follow = 0, true
	m.footer = "resumed · " + r.Title
	(&m).syncViewport()
	return m, nil
}

func replayLines(msg core.Message, w int) []string {
	t := strings.TrimSpace(msg.Text)
	if t == "" {
		return nil
	}
	switch msg.Role {
	case "user":
		return []string{userSt.Render("› " + t)}
	case "assistant":
		return renderMarkdown(t, w)
	}
	return nil
}

func (m model) viewHistory() string {
	title := lipgloss.NewStyle().Bold(true).Render("Chat history")
	lines := []string{title, hintSt.Render("Resume a past conversation (global timeline)."), ""}
	if len(m.histList) == 0 {
		lines = append(lines, "  No saved sessions yet.", "")
	} else {
		for i, h := range m.histList {
			cur := "  "
			if i == m.histCursor {
				cur = todoProgSt.Render("▸ ")
			}
			t := h.Title
			if t == "" {
				t = "(untitled)"
			}
			meta := h.Updated.Local().Format("Jan 2 15:04") + fmt.Sprintf(" · %d turn", h.Turns)
			if h.Turns != 1 {
				meta += "s"
			}
			lines = append(lines, cur+short(t, 56)+hintSt.Render("  "+meta))
		}
	}
	lines = append(lines, "",
		hintSt.Render("↑/↓ move · enter resume · d delete · esc close"))
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center,
		ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}
