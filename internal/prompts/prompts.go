// Package prompts implements reusable prompt templates (a pi-style feature):
// markdown files invoked in the shell as /name [args...].
//
// Templates live in .cheep/prompts/*.md (project) and ~/.cheep/prompts/*.md
// (global); a project template shadows a global one with the same name.
// Substitution: $ARGUMENTS is everything after the name, $1..$9 are the
// whitespace-separated arguments. An optional front-matter block may set
// `description:` (shown in the autocomplete dropdown).
package prompts

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TedHaley/cheep/internal/config"
)

// Template is one prompt template.
type Template struct {
	Name        string
	Description string
	Body        string
}

// List returns the templates visible from workdir, project-first deduped,
// sorted by name.
func List(workdir string) []Template {
	seen := map[string]bool{}
	var out []Template
	dirs := []string{filepath.Join(workdir, ".cheep", "prompts")}
	if home, err := config.Home(); err == nil {
		dirs = append(dirs, filepath.Join(home, "prompts"))
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			if seen[name] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d, e.Name()))
			if err != nil {
				continue
			}
			desc, body := parse(string(b))
			seen[name] = true
			out = append(out, Template{Name: name, Description: desc, Body: body})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the template with the given name, if any.
func Find(workdir, name string) (Template, bool) {
	for _, t := range List(workdir) {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// parse splits an optional front-matter block ("---\nkey: value\n---\n") off
// the body and extracts description.
func parse(s string) (desc, body string) {
	body = s
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if fm, after, ok := strings.Cut(rest, "\n---"); ok {
			for _, line := range strings.Split(fm, "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "description" {
					desc = strings.TrimSpace(v)
				}
			}
			body = strings.TrimPrefix(after, "\n")
		}
	}
	return desc, strings.TrimSpace(body)
}

// Expand substitutes $ARGUMENTS and $1..$9 into the template body. $ARGUMENTS
// is args verbatim; $N is the Nth whitespace-separated field ("" if absent).
// When the body has no placeholders and args are given, they are appended as a
// trailing line so nothing typed is silently dropped.
func Expand(t Template, args string) string {
	args = strings.TrimSpace(args)
	fields := strings.Fields(args)
	out := t.Body
	used := strings.Contains(out, "$ARGUMENTS")
	out = strings.ReplaceAll(out, "$ARGUMENTS", args)
	for i := 9; i >= 1; i-- { // longest first is moot here, but $1 must not eat $10+
		ph := "$" + strconv.Itoa(i)
		if strings.Contains(out, ph) {
			used = true
			v := ""
			if i <= len(fields) {
				v = fields[i-1]
			}
			out = strings.ReplaceAll(out, ph, v)
		}
	}
	if !used && args != "" {
		out += "\n\n" + args
	}
	return out
}
