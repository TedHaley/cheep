// Package skills exposes on-demand knowledge files as tools. An agent lists
// them (cheap: just names + descriptions) and loads a skill's full text only
// when it's relevant — keeping prompts small (and cheap) instead of stuffing
// all knowledge into every request.
//
// Two forms are accepted, searched project-first so a repo can override a
// personal skill of the same name:
//
//	<workdir>/.agents/skills/   and  <workdir>/.claude/skills/   (project)
//	~/.claude/skills/           and  ~/.cheep/skills/            (personal)
//
// A skill is either a flat markdown file (<name>.md) or, per the open Agent
// Skills spec, a directory containing SKILL.md (plus optional supporting
// files). Either form may start with simple front-matter:
//
//	---
//	name: postgres-migrations
//	description: how we write and run DB migrations in this repo
//	---
//	<the knowledge…>
//
// Without front-matter, the name is the file/directory name and the
// description is the first non-empty line.
package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

type Skill struct {
	Name string
	Desc string
	Path string // the markdown file (SKILL.md for directory skills)
	Dir  string // skill directory ("" for flat files)
}

// Dir is the personal cheep skills directory, ~/.cheep/skills.
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "skills"), nil
}

// searchDirs returns the skill directories in precedence order (first hit for
// a name wins). Missing directories are fine; symlinked ones are followed.
func searchDirs(workdir string) []string {
	var dirs []string
	if workdir != "" {
		dirs = append(dirs,
			filepath.Join(workdir, ".agents", "skills"),
			filepath.Join(workdir, ".claude", "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}
	if d, err := Dir(); err == nil {
		dirs = append(dirs, d)
	}
	return dirs
}

// Load reads skill metadata from all search directories (cheap; no bodies).
// Duplicate names keep the earliest (most project-specific) hit.
func Load(workdir string) []Skill {
	seen := map[string]bool{}
	var out []Skill
	for _, d := range searchDirs(workdir) {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		var batch []Skill
		for _, e := range entries {
			var s Skill
			switch {
			case e.IsDir() || e.Type()&os.ModeSymlink != 0 && isDir(filepath.Join(d, e.Name())):
				p := filepath.Join(d, e.Name(), "SKILL.md")
				if _, err := os.Stat(p); err != nil {
					continue
				}
				name, desc := meta(p, e.Name())
				s = Skill{Name: name, Desc: desc, Path: p, Dir: filepath.Join(d, e.Name())}
			case strings.HasSuffix(e.Name(), ".md"):
				p := filepath.Join(d, e.Name())
				name, desc := meta(p, strings.TrimSuffix(e.Name(), ".md"))
				s = Skill{Name: name, Desc: desc, Path: p}
			default:
				continue
			}
			batch = append(batch, s)
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].Name < batch[j].Name })
		for _, s := range batch {
			if !seen[s.Name] {
				seen[s.Name] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func isDir(p string) bool {
	fi, err := os.Stat(p) // follows symlinks
	return err == nil && fi.IsDir()
}

func meta(path, fallback string) (name, desc string) {
	name = fallback
	b, err := os.ReadFile(path)
	if err != nil {
		return name, ""
	}
	text := string(b)
	if strings.HasPrefix(text, "---") {
		if end := strings.Index(text[3:], "---"); end >= 0 {
			for _, line := range strings.Split(text[3:end+3], "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "name":
					if s := strings.TrimSpace(v); s != "" {
						name = s
					}
				case "description", "when", "when_to_use":
					if s := strings.TrimSpace(v); s != "" {
						desc = s
					}
				}
			}
		}
	}
	if desc == "" {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			if line != "" && line != "---" {
				desc = line
				break
			}
		}
	}
	return name, desc
}

// Tools returns list_skills / use_skill for the skills visible from workdir,
// or nil if there are none.
func Tools(workdir string) []core.Tool {
	skills := Load(workdir)
	if len(skills) == 0 {
		return nil
	}
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}
	obj := func(props map[string]any, req ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			m["required"] = req
		}
		return m
	}
	str := map[string]any{"type": "string"}
	return []core.Tool{
		{
			Name:        "list_skills",
			Description: "List available skill knowledge files (name + when to use). Call this, then use_skill(name) to load one's full content before doing relevant work.",
			Parameters:  obj(map[string]any{}),
			Func: func(context.Context, map[string]any) string {
				var list []map[string]string
				for _, sk := range skills {
					list = append(list, map[string]string{"name": sk.Name, "description": sk.Desc})
				}
				b, _ := json.MarshalIndent(map[string]any{"skills": list}, "", "  ")
				return string(b)
			},
		},
		{
			Name:        "use_skill",
			Description: "Load the full content of a skill by name (from list_skills) into context.",
			Parameters:  obj(map[string]any{"name": str}, "name"),
			Func: func(_ context.Context, a map[string]any) string {
				name, _ := a["name"].(string)
				sk, ok := byName[name]
				if !ok {
					return "ERROR: no skill named " + name + " — call list_skills"
				}
				b, err := os.ReadFile(sk.Path)
				if err != nil {
					return "ERROR: " + err.Error()
				}
				body := string(b)
				if sk.Dir != "" {
					if extra := supportingFiles(sk.Dir); len(extra) > 0 {
						body += "\n\n(supporting files in " + sk.Dir + ": " + strings.Join(extra, ", ") + ")"
					}
				}
				return body
			},
		},
	}
}

// supportingFiles lists a directory skill's bundled resources (everything but
// SKILL.md itself), relative to the skill directory.
func supportingFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel != "SKILL.md" {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}
