package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/TedHaley/cheep/internal/core"
)

// LessonTool returns a record_lesson tool that appends a one-line lesson to
// the project's AGENTS.md under "## Lessons", creating the section (or the
// file) if needed. This is how corrections from the user become durable
// project memory instead of repeat mistakes.
func LessonTool(workdir string) core.Tool {
	return core.Tool{
		Name: "record_lesson",
		Description: "Record a durable one-line lesson about this project in AGENTS.md (## Lessons). " +
			"Use when the user corrects you about how the project works — a convention, a command, " +
			"a gotcha — so future sessions don't repeat the mistake.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"lesson": map[string]any{"type": "string",
					"description": "One concise, self-contained sentence stating the lesson."},
			},
			"required": []string{"lesson"},
		},
		Func: func(_ context.Context, args map[string]any) string {
			lesson, _ := args["lesson"].(string)
			lesson = strings.TrimSpace(strings.ReplaceAll(lesson, "\n", " "))
			if lesson == "" {
				return `ERROR: "lesson" is required`
			}
			path, err := AppendLesson(workdir, lesson)
			if err != nil {
				return "ERROR: " + err.Error()
			}
			return "recorded in " + path
		},
	}
}

// AppendLesson adds "- <lesson>" under the "## Lessons" heading of the
// project's AGENTS.md, creating the heading or the file as needed, and
// returns the path written.
func AppendLesson(workdir, lesson string) (string, error) {
	root, path, body := findLocal(workdir)
	if path == "" {
		root = workdir
		path = filepath.Join(root, "AGENTS.md")
	} else if strings.EqualFold(filepath.Base(path), "CLAUDE.md") {
		// Never append to CLAUDE.md: it is conventionally a symlink to
		// AGENTS.md, and when it isn't, AGENTS.md is still the standard home.
		agents := filepath.Join(root, "AGENTS.md")
		if b, err := os.ReadFile(agents); err == nil {
			path, body = agents, string(b)
		} else {
			path, body = agents, ""
		}
	}

	entry := "- " + lesson
	lines := strings.Split(body, "\n")
	head := -1 // line index of the "## Lessons" heading
	for i, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil &&
			strings.EqualFold(strings.TrimSpace(m[2]), "lessons") {
			head = i
			break
		}
	}
	if head < 0 {
		out := strings.TrimRight(body, "\n")
		if out != "" {
			out += "\n"
		}
		out += "\n## Lessons\n" + entry + "\n"
		return path, os.WriteFile(path, []byte(out), 0o644)
	}
	// Insert at the end of the section: before the next heading, skipping back
	// over any blank lines so the entry joins the existing list.
	end := len(lines)
	for i := head + 1; i < len(lines); i++ {
		if headingRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	insert := end
	for insert > head+1 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	lines = append(lines[:insert], append([]string{entry}, lines[insert:]...)...)
	return path, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}
