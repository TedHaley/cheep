package tui

// Input completion: a filtered dropdown for slash commands (type "/") and
// fuzzy @-file mentions (type "@" + a few characters). Tab/enter accepts,
// up/down navigates, esc dismisses. The registry below is the single source
// of truth for command names shared by completion and dispatch.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type slashCmd struct{ name, help string }

var slashCmds = []slashCmd{
	{"/chat", "talk only, no tools"},
	{"/plan", "read-only investigation"},
	{"/auto", "full autonomy (delegate/edit)"},
	{"/mode", "cycle chat → plan → auto"},
	{"/model", "show or switch the orchestrator model"},
	{"/approval", "gate risky tool calls: yolo | auto | approve"},
	{"/config", "set up agents from discovered servers + keys"},
	{"/setup", "configure by chatting with a working agent"},
	{"/status", "show current setup"},
	{"/tokens", "token usage + estimated $ per model"},
	{"/budget", "set a session $ cap"},
	{"/delivery", "how validated work lands: merge | pr"},
	{"/history", "browse and resume past conversations"},
	{"/resume", "browse and resume past conversations"},
	{"/fork", "branch this conversation from an earlier turn"},
	{"/tree", "show the session tree (forks nested)"},
	{"/keeptabs", "toggle auto-close of finished executor tabs"},
	{"/close", "close the focused executor tab"},
	{"/clear", "reset the conversation"},
	{"/prompts", "list prompt templates (invoke as /name)"},
	{"/help", "show help"},
	{"/exit", "quit cheep"},
}

// slashOptions merges the built-in commands with the workspace's prompt
// templates, so /name templates complete like native commands.
func (m *model) slashOptions() []slashCmd {
	out := append([]slashCmd{}, slashCmds...)
	for _, t := range m.promptTpls {
		desc := t.Description
		if desc == "" {
			desc = "prompt template"
		}
		out = append(out, slashCmd{"/" + t.Name, desc})
	}
	return out
}

type compState struct {
	kind  string // "slash" | "file"
	opts  []string
	idx   int
	token string // the partial text being completed
}

// updateCompletions recomputes the dropdown from the current input value.
func (m *model) updateCompletions() {
	m.comp = compState{}
	val := m.input.Value()
	if val == "" {
		return
	}
	if strings.HasPrefix(val, "/") && !strings.ContainsAny(val, " \n") {
		var opts []string
		for _, c := range m.slashOptions() {
			if strings.HasPrefix(c.name, val) {
				opts = append(opts, c.name)
			}
		}
		if len(opts) == 1 && opts[0] == val {
			return // fully typed — nothing to offer
		}
		if len(opts) > 0 {
			m.comp = compState{kind: "slash", opts: opts, token: val}
		}
		return
	}
	// @-file mention: complete the token under the cursor's end of input.
	if strings.HasSuffix(val, " ") || strings.HasSuffix(val, "\n") {
		return
	}
	fields := strings.Fields(val)
	if len(fields) == 0 {
		return
	}
	last := fields[len(fields)-1]
	if !strings.HasPrefix(last, "@") || len(last) < 2 {
		return
	}
	if opts := fuzzyFilter(m.fileList, last[1:], 8); len(opts) > 0 {
		m.comp = compState{kind: "file", opts: opts, token: last}
	}
}

// acceptCompletion applies the highlighted option. Returns the kind accepted
// ("" if none).
func (m *model) acceptCompletion() string {
	if len(m.comp.opts) == 0 {
		return ""
	}
	opt := m.comp.opts[m.comp.idx]
	kind := m.comp.kind
	val := m.input.Value()
	switch kind {
	case "slash":
		m.input.SetValue(opt)
	case "file":
		m.input.SetValue(val[:len(val)-len(m.comp.token)] + opt + " ")
	}
	m.input.CursorEnd()
	m.comp = compState{}
	return kind
}

func (m model) viewCompletions() string {
	if len(m.comp.opts) == 0 {
		return ""
	}
	help := map[string]string{}
	for _, c := range (&m).slashOptions() {
		help[c.name] = c.help
	}
	sel := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	var lines []string
	for i, o := range m.comp.opts {
		line := "  " + o
		if h := help[o]; h != "" && m.comp.kind == "slash" {
			line += hintSt.Render("  — " + h)
		}
		if i == m.comp.idx {
			line = sel.Render("▸ " + o)
			if h := help[o]; h != "" && m.comp.kind == "slash" {
				line += hintSt.Render("  — " + h)
			}
		}
		lines = append(lines, line)
	}
	lines = append(lines, hintSt.Render("  tab/enter: accept · esc: dismiss"))
	return strings.Join(lines, "\n")
}

// loadFileList indexes workspace files for @-mentions: git ls-files when
// available, else a bounded walk.
func loadFileList(workdir string) []string {
	if out, err := exec.Command("git", "-C", workdir, "ls-files").Output(); err == nil {
		files := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(files) > 0 && files[0] != "" {
			return files
		}
	}
	var files []string
	root := filepath.Clean(workdir)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(files) >= 2000 {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == "target" || name == ".venv" ||
				strings.Count(strings.TrimPrefix(p, root), string(filepath.Separator)) > 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// fuzzyFilter ranks paths: substring of the basename beats substring of the
// path beats a subsequence match; shorter paths win ties.
func fuzzyFilter(list []string, q string, limit int) []string {
	q = strings.ToLower(q)
	type scored struct {
		path string
		rank int
	}
	var hits []scored
	for _, p := range list {
		lp := strings.ToLower(p)
		switch {
		case strings.Contains(strings.ToLower(filepath.Base(p)), q):
			hits = append(hits, scored{p, 0})
		case strings.Contains(lp, q):
			hits = append(hits, scored{p, 1})
		case isSubsequence(q, lp):
			hits = append(hits, scored{p, 2})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		return len(hits[i].path) < len(hits[j].path)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.path
	}
	return out
}

func isSubsequence(q, s string) bool {
	if q == "" {
		return false
	}
	i := 0
	for j := 0; j < len(s) && i < len(q); j++ {
		if s[j] == q[i] {
			i++
		}
	}
	return i == len(q)
}
