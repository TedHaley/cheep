// Package tool provides the built-in tools available to agents.
//
// All tools are scoped to a workspace directory. The orchestrator gets the
// read-only subset (so it can verify, e.g. run tests) plus delegation; executors
// also get write_file so they can do the work.
//
// NOTE: run_bash executes arbitrary shell commands with no sandbox. That is
// intentional for an autonomous coding agent, but it means you should only point
// cheep at a workspace you trust it to modify.
package tool

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

const maxOutput = 8000

func truncate(s string) string {
	if len(s) > maxOutput {
		return s[:maxOutput] + fmt.Sprintf("\n... [truncated %d chars]", len(s)-maxOutput)
	}
	return s
}

// confine resolves path within workdir and refuses anything that escapes it.
// This is what makes worktree isolation real: an executor cannot read or write
// outside the workspace it was given (absolute paths and ".." are rejected).
func confine(workdir, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute paths are not allowed; use a path relative to the workspace")
	}
	full := filepath.Join(workdir, path)
	rel, err := filepath.Rel(workdir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", path)
	}
	return full, nil
}

func argStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// Make builds the tool set for a workspace. includeWrite adds write_file.
func Make(workdir string, includeWrite bool) []core.Tool {
	if abs, err := filepath.Abs(workdir); err == nil {
		workdir = abs
	}

	readFile := func(_ context.Context, args map[string]any) string {
		p, err := confine(workdir, argStr(args, "path"))
		if err != nil {
			return "ERROR: " + err.Error()
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return truncate(string(b))
	}

	listDir := func(_ context.Context, args map[string]any) string {
		rel := argStr(args, "path")
		if rel == "" {
			rel = "."
		}
		p, err := confine(workdir, rel)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		var out bytes.Buffer
		for _, e := range entries {
			out.WriteString(e.Name())
			out.WriteByte('\n')
		}
		if out.Len() == 0 {
			return "(empty)"
		}
		return out.String()
	}

	runBash := func(parent context.Context, args map[string]any) string {
		command := argStr(args, "command")
		timeout := 120 * time.Second
		if t, ok := args["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t) * time.Second
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = workdir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "ERROR: command timed out"
			}
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		return truncate(fmt.Sprintf("exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s",
			code, stdout.String(), stderr.String()))
	}

	writeFile := func(_ context.Context, args map[string]any) string {
		rel := argStr(args, "path")
		p, err := confine(workdir, rel)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "ERROR: " + err.Error()
		}
		content := argStr(args, "content")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("wrote %d chars to %s", len(content), rel)
	}

	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	str := map[string]any{"type": "string"}

	tools := []core.Tool{
		{
			Name:        "read_file",
			Description: "Read a UTF-8 text file relative to the workspace.",
			Parameters:  obj(map[string]any{"path": str}, "path"),
			Func:        readFile,
		},
		{
			Name:        "list_dir",
			Description: "List the entries of a directory relative to the workspace.",
			Parameters:  obj(map[string]any{"path": str}),
			Func:        listDir,
		},
		{
			Name:        "run_bash",
			Description: "Run a shell command in the workspace; returns exit code, stdout and stderr.",
			Parameters: obj(map[string]any{
				"command": str,
				"timeout": map[string]any{"type": "integer", "description": "seconds, default 120"},
			}, "command"),
			Func: runBash,
		},
	}
	if includeWrite {
		tools = append(tools, core.Tool{
			Name:        "write_file",
			Description: "Create or overwrite a text file relative to the workspace.",
			Parameters:  obj(map[string]any{"path": str, "content": str}, "path", "content"),
			Func:        writeFile,
		})
	}

	// update_todos is a UI/planning tool: it has no side effect, but the shell
	// renders the checklist and checks items off as their status changes.
	tools = append(tools, core.Tool{
		Name: "update_todos",
		Description: "Maintain a checklist of the steps for the current task. Call it when you " +
			"plan the work and again whenever a step starts or finishes. Always send the FULL list.",
		Parameters: obj(map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": obj(map[string]any{
					"title":  str,
					"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done"}},
				}, "title", "status"),
			},
		}, "todos"),
		Func: func(_ context.Context, args map[string]any) string {
			n := 0
			if ts, ok := args["todos"].([]any); ok {
				n = len(ts)
			}
			return fmt.Sprintf("todo list updated (%d items)", n)
		},
	})
	return tools
}
