// Command cheep-claude-mcp is an EXPERIMENTAL stdio MCP server that exposes one
// tool, ask_claude, which delegates a self-contained task to headless Claude Code
// (`claude -p`). Because it shells out to the official Claude Code CLI, it uses
// whatever that CLI is logged in with — including a Pro/Max subscription — so no
// Anthropic API key is needed.
//
// Wire it into cheep via ~/.cheep/config.json:
//
//	"mcp": {
//	  "claude": { "command": "cheep-claude-mcp", "roles": ["orchestrator"] }
//	}
//
// The orchestrator then gets a `claude__ask_claude` tool it can delegate hard
// work to, while cheap/local agents do the rest.
//
// CAVEATS: programmatically driving a subscription via headless Claude Code is a
// gray area subject to Anthropic's usage policy and Max-plan rate limits; it is
// unsupported and may break. Use at your own risk.
//
// Env knobs:
//
//	CHEEP_CLAUDE_BIN      claude binary (default "claude")
//	CHEEP_CLAUDE_ARGS     extra flags, space-separated (e.g. "--model sonnet --permission-mode acceptEdits")
//	CHEEP_CLAUDE_TIMEOUT  seconds before a task is aborted (default 600)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const protocolVersion = "2024-11-05"

var askClaude = map[string]any{
	"name": "ask_claude",
	"description": "Delegate a self-contained task to Claude (via the logged-in Claude Code " +
		"subscription — no API key). Returns Claude's result as text. Use for heavy reasoning, " +
		"planning, or hard changes; keep routine work on the local agents. Make the task fully " +
		"self-contained — Claude does not see this conversation.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{"type": "string", "description": "The complete, self-contained instruction for Claude."},
			"cwd":  map[string]any{"type": "string", "description": "Optional working directory to run the task in."},
		},
		"required": []string{"task"},
	},
}

func main() {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for {
		line, err := in.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			handle(enc, t)
		}
		if err != nil {
			return // EOF: cheep closed the pipe
		}
	}
}

type request struct {
	ID     json.RawMessage `json:"id"` // absent for notifications
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func handle(enc *json.Encoder, line []byte) {
	var req request
	if json.Unmarshal(line, &req) != nil {
		return
	}
	switch req.Method {
	case "initialize":
		reply(enc, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "cheep-claude-mcp", "version": "0.1.0"},
		})
	case "notifications/initialized":
		// notification — no response
	case "ping":
		reply(enc, req.ID, map[string]any{})
	case "tools/list":
		reply(enc, req.ID, map[string]any{"tools": []any{askClaude}})
	case "tools/call":
		handleCall(enc, req.ID, req.Params)
	default:
		if len(req.ID) > 0 {
			replyErr(enc, req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func handleCall(enc *json.Encoder, id, params json.RawMessage) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Name != "ask_claude" {
		textResult(enc, id, "ERROR: unknown tool "+p.Name, true)
		return
	}
	task, _ := p.Arguments["task"].(string)
	cwd, _ := p.Arguments["cwd"].(string)
	if strings.TrimSpace(task) == "" {
		textResult(enc, id, "ERROR: task is required", true)
		return
	}
	out, err := runClaude(task, cwd)
	if err != nil {
		textResult(enc, id, "ERROR: "+err.Error()+"\n"+out, true)
		return
	}
	textResult(enc, id, out, false)
}

func runClaude(task, cwd string) (string, error) {
	bin := envOr("CHEEP_CLAUDE_BIN", "claude")
	timeout := 600
	if v, err := strconv.Atoi(os.Getenv("CHEEP_CLAUDE_TIMEOUT")); err == nil && v > 0 {
		timeout = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	args := []string{"-p", task, "--output-format", "json"}
	if extra := strings.Fields(os.Getenv("CHEEP_CLAUDE_ARGS")); len(extra) > 0 {
		args = append(args, extra...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("claude timed out after %ds", timeout)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return msg, fmt.Errorf("claude exited: %v", err)
	}
	if txt, ok := extractResult(stdout.Bytes()); ok {
		return txt, nil
	}
	return strings.TrimSpace(stdout.String()), nil
}

// extractResult pulls the final text out of `claude -p --output-format json`,
// which returns an object with a "result" string.
func extractResult(b []byte) (string, bool) {
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil {
		if r, ok := obj["result"].(string); ok {
			return strings.TrimSpace(r), true
		}
	}
	return "", false
}

func textResult(enc *json.Encoder, id json.RawMessage, text string, isErr bool) {
	reply(enc, id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	})
}

func reply(enc *json.Encoder, id json.RawMessage, result any) {
	if len(id) == 0 {
		return // notification: never reply
	}
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func replyErr(enc *json.Encoder, id json.RawMessage, code int, message string) {
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
