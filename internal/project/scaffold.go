package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scaffold writes a project's launchpad files into dir: an AGENTS.md seeded
// with detected commands, a CLAUDE.md symlink to it, and the cheep-toolbelt
// skill that teaches other harnesses cheep's headless subcommands. Existing
// files are left alone unless force is set (the symlink and skill are only
// ever created, never forced over user content). Returns the paths written.
func Scaffold(dir string, d Detected, force bool) (wrote []string, err error) {
	agents := filepath.Join(dir, "AGENTS.md")
	if _, statErr := os.Stat(agents); os.IsNotExist(statErr) || force {
		if err := os.WriteFile(agents, []byte(agentsTemplate(dir, d)), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, agents)
	}

	claude := filepath.Join(dir, "CLAUDE.md")
	if _, statErr := os.Lstat(claude); os.IsNotExist(statErr) {
		if err := os.Symlink("AGENTS.md", claude); err != nil {
			// Filesystems without symlinks (FAT, some Windows setups): a
			// pointer file keeps Claude-flavored tools working.
			if werr := os.WriteFile(claude, []byte("See @AGENTS.md — the canonical instructions file.\n"), 0o644); werr != nil {
				return wrote, werr
			}
		}
		wrote = append(wrote, claude)
	}

	skill := filepath.Join(dir, ".agents", "skills", "cheep-toolbelt", "SKILL.md")
	if _, statErr := os.Stat(skill); os.IsNotExist(statErr) {
		if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(skill, []byte(toolbeltSkill), 0o644); err != nil {
			return wrote, err
		}
		wrote = append(wrote, skill)
	}
	return wrote, nil
}

func agentsTemplate(dir string, d Detected) string {
	name := filepath.Base(absOr(dir))
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", name)
	b.WriteString("<!-- One paragraph: what this project is and does. Agents read this first. -->\n\n")

	b.WriteString("## Build & Run\n\n")
	if len(d.Build) > 0 {
		for _, c := range d.Build {
			b.WriteString("- Build: `" + c + "`\n")
		}
	} else {
		b.WriteString("<!-- How to build and run the app locally, including any setup. -->\n")
	}
	b.WriteString("\n")

	b.WriteString("## Validation\n\n")
	b.WriteString("Commands that must pass before work is merged. cheep runs every fenced\n" +
		"`check` block below on each agent's changes (`cheep validate` runs them by hand).\n\n")
	for i, c := range append(append([]string{}, d.Test...), d.Lint...) {
		name := fmt.Sprintf("check-%d", i+1)
		switch {
		case strings.Contains(c, "test"):
			name = "test"
		case strings.Contains(c, "lint") || strings.Contains(c, "vet") || strings.Contains(c, "clippy") || strings.Contains(c, "ruff"):
			name = "lint"
		case strings.Contains(c, "typecheck"):
			name = "typecheck"
		}
		fmt.Fprintf(&b, "```check name=%s\n%s\n```\n\n", name, c)
	}
	if len(d.Test)+len(d.Lint) == 0 {
		b.WriteString("```check name=test\n# TODO: the project's test command, e.g. go test ./...\ntrue\n```\n\n")
	}
	b.WriteString("<!-- Also describe how to exercise the app END TO END (dev server command,\n" +
		"     test user, seed data) — unit tests alone are not evidence a feature works. -->\n\n")

	b.WriteString("## Conventions\n\n" +
		"<!-- Code style, layout, and decisions an agent can't infer from the code. -->\n\n")

	b.WriteString("## Lessons\n\n" +
		"<!-- Agents append corrections here via record_lesson; keep entries one line. -->\n")
	return b.String()
}

func absOr(dir string) string {
	if a, err := filepath.Abs(dir); err == nil {
		return a
	}
	return dir
}

const toolbeltSkill = `---
name: cheep-toolbelt
description: Use when validating changes before merge/PR or when running parallel work in isolated git worktrees in this repo — cheep provides both as headless commands.
---

# cheep toolbelt

This repo is set up for [cheep](https://github.com/TedHaley/cheep). Two of its
subcommands are useful from ANY agent harness:

## Validate changes before merging

` + "```sh\ncheep validate --json          # runs the '## Validation' checks from AGENTS.md\ncheep validate --review --json # + a fresh-context AI review of your branch diff\n```" + `

Exit 0 = pass, 1 = fail (read the JSON for which check/issue), 2 = config error.
Run it before declaring work done or opening a PR.

## Isolated worktrees for parallel work

` + "```sh\ncheep worktree acquire --name <task> --json   # → {path, branch, base}; cd into path\ncheep worktree release --path <p> --landed    # after the branch is merged\ncheep worktree release --path <p>             # NOT merged: slot is quarantined, work kept\ncheep worktree list --json\n```" + `

Slots reuse cached build artifacts (node_modules, target/) between tasks, and a
slot holding unmerged commits is never recycled — release without --landed is
always safe.
`
