package project

import (
	"regexp"
	"strings"
)

// Check is one validation command declared in AGENTS.md. Script may be a
// multi-line shell script; it passes when it exits 0.
type Check struct {
	Name   string
	Script string
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s*(.*?)\s*#*\s*$`)
	fenceRe   = regexp.MustCompile("^(```+|~~~+)\\s*(.*)$")
	bulletRe  = regexp.MustCompile("^[-*]\\s+`([^`]+)`\\s*$")
	nameRe    = regexp.MustCompile(`(?:^|\s)name=([\w.-]+)`)
)

// ParseChecks extracts checks from a markdown document's "Validation" section
// (any heading level, case-insensitive, e.g. "## Validation"). Inside it, each
// fenced code block whose info string is empty or starts with check/sh/bash/
// shell is one check (optionally named via `name=<word>` in the info string),
// and each bullet holding a single inline code span (- `go test ./...`) is one
// check. Prose is ignored. The section ends at the next heading of equal or
// shallower depth.
func ParseChecks(md string) []Check {
	if md == "" {
		return nil
	}
	var checks []Check
	lines := strings.Split(md, "\n")
	depth := 0            // heading depth of the Validation section; 0 = not in it
	var fence string      // the opening fence marker when inside a code block
	var fenceName string  // name for the current fenced check ("" = not a check block)
	var script []string   // accumulated lines of the current fenced check
	anon := 0             // counter for unnamed checks

	flush := func() {
		if fenceName == "" {
			return
		}
		s := strings.TrimSpace(strings.Join(script, "\n"))
		if s != "" {
			checks = append(checks, Check{Name: fenceName, Script: s})
		}
		fenceName, script = "", nil
	}

	for _, line := range lines {
		if fence != "" { // inside a fenced block
			if strings.HasPrefix(strings.TrimSpace(line), fence) {
				flush()
				fence = ""
				continue
			}
			if fenceName != "" {
				script = append(script, line)
			}
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			d := len(m[1])
			title := strings.ToLower(m[2])
			switch {
			case strings.HasPrefix(title, "validation"):
				depth = d
			case depth > 0 && d <= depth:
				depth = 0 // left the section
			}
			continue
		}
		if depth == 0 {
			continue
		}
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			fence = m[1][:3]
			info := strings.TrimSpace(m[2])
			lang, _, _ := strings.Cut(info, " ")
			switch strings.ToLower(lang) {
			case "", "check", "sh", "bash", "shell":
				anon++
				fenceName = "check-" + itoa(anon)
				if nm := nameRe.FindStringSubmatch(info); nm != nil {
					fenceName = nm[1]
				}
			default:
				fenceName = "" // some other language — not a check
			}
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			anon++
			checks = append(checks, Check{Name: "check-" + itoa(anon), Script: m[1]})
		}
	}
	flush() // unterminated fence at EOF
	return checks
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
