package tui

// The /scheduled overlay: view and modify recurring jobs (internal/jobs).
// space toggles enabled, d deletes, r runs the job's task now in this session,
// enter/esc closes.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TedHaley/cheep/internal/jobs"
)

func (m model) openScheduled() (tea.Model, tea.Cmd) {
	list, _ := jobs.List()
	m.jobsList = list
	m.jobsCursor = 0
	m.overlay = "scheduled"
	return m, nil
}

func (m model) updateScheduled(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc", "q", "enter":
		m.overlay = ""
	case "up", "k":
		if m.jobsCursor > 0 {
			m.jobsCursor--
		}
	case "down", "j":
		if m.jobsCursor < len(m.jobsList)-1 {
			m.jobsCursor++
		}
	case " ":
		if m.jobsCursor < len(m.jobsList) {
			j := m.jobsList[m.jobsCursor]
			j.Enabled = !j.Enabled
			_ = jobs.Save(j)
			m.jobsList[m.jobsCursor] = j
		}
	case "d":
		if m.jobsCursor < len(m.jobsList) {
			_ = jobs.Remove(m.jobsList[m.jobsCursor].ID)
			m.jobsList = append(m.jobsList[:m.jobsCursor], m.jobsList[m.jobsCursor+1:]...)
			if m.jobsCursor >= len(m.jobsList) && m.jobsCursor > 0 {
				m.jobsCursor--
			}
		}
	case "r":
		if m.jobsCursor < len(m.jobsList) {
			j := m.jobsList[m.jobsCursor]
			m.overlay = ""
			return m.sendUser(j.Task, userLine(j.Task, m.w))
		}
	}
	return m, nil
}

func (m model) viewScheduled() string {
	title := lipgloss.NewStyle().Bold(true).Render("Scheduled jobs")
	lines := []string{title, hintSt.Render("Recurring tasks — fire when `cheep daemon` is running."), ""}
	if len(m.jobsList) == 0 {
		lines = append(lines, "  No scheduled jobs yet.",
			hintSt.Render("  Ask in the shell: \"run the test suite every weekday at 9am\"."), "")
	} else {
		now := time.Now()
		for i, j := range m.jobsList {
			cur := "  "
			if i == m.jobsCursor {
				cur = todoProgSt.Render("▸ ")
			}
			state := okSt.Render("● on ")
			if !j.Enabled {
				state = hintSt.Render("○ off")
			}
			name := j.Name
			if name == "" {
				name = j.ID
			}
			meta := "  " + j.Schedule
			if n, ok := j.Next(now); ok && j.Enabled {
				meta += hintSt.Render("  · next " + n.Local().Format("Mon 15:04"))
			}
			if !j.LastRun.IsZero() {
				meta += hintSt.Render("  · last " + j.LastStatus)
			}
			lines = append(lines, cur+state+"  "+userSt.Render(short(name, 22))+
				hintSt.Render("  "+short(strings.ReplaceAll(j.Task, "\n", " "), 40))+meta)
		}
	}
	lines = append(lines, "",
		hintSt.Render("↑/↓ move · space on/off · r run now · d delete · esc close"))
	return m.place(ovBox.Padding(1, 2).Render(strings.Join(lines, "\n")))
}

var _ = fmt.Sprint
