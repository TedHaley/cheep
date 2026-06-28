// Package mcp is a minimal Model Context Protocol client for stdio servers.
//
// It spawns each configured server, performs the JSON-RPC initialize handshake,
// lists the server's tools, and adapts each one into a core.Tool whose Func
// issues a tools/call request. Those tools can then be handed to any agent —
// the agent loop and providers treat them like any other tool.
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/TedHaley/cheep/internal/core"
)

// Server is one stdio MCP server (a command cheep launches).
type Server struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Session owns the running MCP servers; Close shuts them all down.
type Session struct{ clients []*client }

func (s *Session) Close() {
	for _, c := range s.clients {
		c.close()
	}
}

// Start launches every server, returns the adapted tools and a Session to close.
// Servers that fail to start are reported via onEvent and skipped.
func Start(servers map[string]Server, onEvent core.EventFunc) ([]core.Tool, *Session) {
	sess := &Session{}
	var tools []core.Tool
	emit := func(s string) {
		if onEvent != nil {
			onEvent(core.Event{Agent: "cheep", Type: "status", Status: s})
		}
	}
	for name, srv := range servers {
		c, err := startClient(srv)
		if err != nil {
			emit(fmt.Sprintf("mcp %q failed to start: %v", name, err))
			continue
		}
		mts, err := c.listTools()
		if err != nil {
			emit(fmt.Sprintf("mcp %q tools/list failed: %v", name, err))
			c.close()
			continue
		}
		sess.clients = append(sess.clients, c)
		for _, mt := range mts {
			params := mt.InputSchema
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			cc, real := c, mt.Name
			tools = append(tools, core.Tool{
				Name:        sanitize(name) + "__" + sanitize(mt.Name),
				Description: "[" + name + "] " + mt.Description,
				Parameters:  params,
				Func:        func(args map[string]any) string { return cc.callTool(real, args) },
			})
		}
		emit(fmt.Sprintf("mcp %q: %d tool(s)", name, len(mts)))
	}
	return tools, sess
}

// ---- JSON-RPC stdio client ------------------------------------------------

type client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	mu     sync.Mutex // serializes request/response round-trips
	nextID int
}

type rpcResp struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func startClient(s Server) (*client, error) {
	cmd := exec.Command(s.Command, s.Args...)
	if len(s.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range s.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &client{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}

	if _, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cheep", "version": "0.0.1"},
	}); err != nil {
		c.close()
		return nil, err
	}
	c.notify("notifications/initialized", map[string]any{})
	return c, nil
}

func (c *client) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	req, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if _, err := c.stdin.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if line = bytes.TrimSpace(line); len(line) == 0 {
			continue
		}
		var resp rpcResp
		if json.Unmarshal(line, &resp) != nil || resp.ID == nil || *resp.ID != id {
			continue // notification / log / other id
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *client) notify(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	_, _ = c.stdin.Write(append(b, '\n'))
}

func (c *client) listTools() ([]mcpTool, error) {
	raw, err := c.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

func (c *client) callTool(name string, args map[string]any) string {
	raw, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(raw, &out)
	var sb strings.Builder
	for _, part := range out.Content {
		if part.Type == "text" {
			sb.WriteString(part.Text)
			sb.WriteByte('\n')
		}
	}
	s := strings.TrimSpace(sb.String())
	if s == "" {
		s = string(raw)
	}
	if out.IsError {
		return "ERROR: " + s
	}
	return s
}

func (c *client) close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}
