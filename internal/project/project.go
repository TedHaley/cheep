// Package project reads a repository's self-description: the AGENTS.md
// instruction files (global and per-project), the machine-readable validation
// checks declared inside them, and the growing "## Lessons" record.
//
// AGENTS.md is the open cross-harness standard (CLAUDE.md is accepted as a
// fallback for Claude-flavored repos, and is typically a symlink to AGENTS.md).
// Everything cheep knows about a project that isn't derivable from the code
// flows through here, so the orchestrator, executors, and the validation
// pipeline all share one view of the project's rules.
package project

import (
	"os"
	"path/filepath"
	"strings"
)

// maxPromptBytes bounds how much instruction text is injected into system
// prompts; beyond this, files are clipped (they should be short anyway).
const maxPromptBytes = 16 * 1024

// Context is a project's instruction set, resolved from workdir.
type Context struct {
	Root      string  // directory containing the project file ("" if none found)
	LocalPath string  // the AGENTS.md/CLAUDE.md actually used ("" if none)
	Global    string  // contents of ~/AGENTS.md ("" if absent)
	Local     string  // contents of the project file ("" if absent)
	Checks    []Check // parsed from the project file's "## Validation" section
}

// Load resolves the instruction files for workdir. It never fails: missing
// files simply leave fields empty. The project file is found by walking up
// from workdir to the repository root (the first directory containing .git),
// or just workdir itself outside a repo; AGENTS.md wins over CLAUDE.md.
func Load(workdir string) Context {
	var c Context
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, "AGENTS.md")); err == nil {
			c.Global = string(b)
		}
	}
	c.Root, c.LocalPath, c.Local = findLocal(workdir)
	c.Checks = ParseChecks(c.Local)
	return c
}

// findLocal walks from workdir up to (and including) the git root looking for
// AGENTS.md, falling back to CLAUDE.md at each level.
func findLocal(workdir string) (root, path, body string) {
	dir, err := filepath.Abs(workdir)
	if err != nil {
		dir = workdir
	}
	for {
		for _, name := range [...]string{"AGENTS.md", "CLAUDE.md"} {
			p := filepath.Join(dir, name)
			if b, err := os.ReadFile(p); err == nil {
				return dir, p, string(b)
			}
		}
		atRepoRoot := false
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			atRepoRoot = true
		}
		parent := filepath.Dir(dir)
		if atRepoRoot || parent == dir {
			return "", "", ""
		}
		dir = parent
	}
}

// PromptBlock renders the instructions as a system-prompt section, or "" when
// there are none. Global instructions come first, the project's second, so the
// more specific file reads as the later (and effectively overriding) word.
func (c Context) PromptBlock() string {
	if c.Global == "" && c.Local == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Project instructions (AGENTS.md)\n" +
		"Follow these standing instructions from the user. The project-level section takes " +
		"precedence over the global one where they disagree.\n")
	if g := strings.TrimSpace(c.Global); g != "" {
		b.WriteString("\n## Global (~/AGENTS.md)\n" + clip(g, maxPromptBytes/2) + "\n")
	}
	if l := strings.TrimSpace(c.Local); l != "" {
		b.WriteString("\n## This project (" + filepath.Base(c.LocalPath) + ")\n" + clip(l, maxPromptBytes) + "\n")
	}
	return b.String()
}

// InstructionsBlock is a convenience for callers that only need the rendered
// prompt section (e.g. executor sessions, which re-read per session so fresh
// lessons and uncommitted AGENTS.md edits are picked up).
func InstructionsBlock(workdir string) string {
	return Load(workdir).PromptBlock()
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n…(clipped)"
	}
	return s
}
