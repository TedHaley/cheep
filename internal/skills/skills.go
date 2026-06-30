// Package skills exposes on-demand knowledge files (~/.cheep/skills/*.md) as
// tools. An agent lists them (cheap: just names + descriptions) and loads a
// skill's full text only when it's relevant — keeping prompts small (and cheap)
// instead of stuffing all knowledge into every request.
//
// A skill file may start with simple front-matter:
//
//	---
//	name: postgres-migrations
//	description: how we write and run DB migrations in this repo
//	---
//	<the knowledge…>
//
// Without front-matter, the name is the filename and the description is the first
// non-empty line.
package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/TedHaley/cheep/internal/config"
	"github.com/TedHaley/cheep/internal/core"
)

type Skill struct {
	Name string
	Desc string
	Path string
}

// Dir is ~/.cheep/skills.
func Dir() (string, error) {
	h, err := config.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "skills"), nil
}

// Load reads skill metadata from the skills directory (cheap; no bodies).
func Load() []Skill {
	d, err := Dir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(d, e.Name())
		name, desc := meta(p, e.Name())
		out = append(out, Skill{Name: name, Desc: desc, Path: p})
	}
	return out
}

func meta(path, file string) (name, desc string) {
	name = strings.TrimSuffix(file, ".md")
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

// Tools returns list_skills / use_skill, or nil if there are no skills.
func Tools() []core.Tool {
	skills := Load()
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
				return string(b)
			},
		},
	}
}
