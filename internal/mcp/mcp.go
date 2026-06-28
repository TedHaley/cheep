// Package mcp is a minimal Model Context Protocol client supporting stdio and
// HTTP (Streamable HTTP / SSE) servers.
//
// It connects to each configured server, performs the JSON-RPC initialize
// handshake, lists tools, and adapts each into a core.Tool. Each server can be
// scoped to the orchestrator, the executors, or both (default).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

// Server is one MCP server. Set Command (+Args/Env) for stdio, or URL (+Headers)
// for HTTP. Roles limits which agents get its tools ("orchestrator"/"executor");
// empty means both.
type Server struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Roles   []string          `json:"roles,omitempty"`
}

// Tools holds the discovered tools split by which agents should receive them.
type Tools struct {
	Orchestrator []core.Tool
	Executor     []core.Tool
}

type Session struct{ clients []rpc }

func (s *Session) Close() {
	for _, c := range s.clients {
		c.close()
	}
}

var initParams = map[string]any{
	"protocolVersion": "2024-11-05",
	"capabilities":    map[string]any{},
	"clientInfo":      map[string]any{"name": "cheep", "version": "0.0.1"},
}

// Start launches every server and returns the role-scoped tools + a Session.
func Start(servers map[string]Server, onEvent core.EventFunc) (Tools, *Session) {
	sess := &Session{}
	var mt Tools
	emit := func(s string) {
		if onEvent != nil {
			onEvent(core.Event{Agent: "cheep", Type: "status", Status: s})
		}
	}
	for name, srv := range servers {
		c, err := dial(srv)
		if err != nil {
			emit(fmt.Sprintf("mcp %q failed to start: %v", name, err))
			continue
		}
		mts, err := listTools(c)
		if err != nil {
			emit(fmt.Sprintf("mcp %q tools/list failed: %v", name, err))
			c.close()
			continue
		}
		sess.clients = append(sess.clients, c)
		toOrch, toExec := hasRole(srv.Roles, "orchestrator"), hasRole(srv.Roles, "executor")
		for _, m := range mts {
			t := adapt(name, c, m)
			if toOrch {
				mt.Orchestrator = append(mt.Orchestrator, t)
			}
			if toExec {
				mt.Executor = append(mt.Executor, t)
			}
		}
		emit(fmt.Sprintf("mcp %q: %d tool(s)", name, len(mts)))
	}
	return mt, sess
}

func hasRole(roles []string, r string) bool {
	if len(roles) == 0 {
		return true // default: both roles
	}
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

func dial(s Server) (rpc, error) {
	if s.URL != "" {
		return startHTTP(s)
	}
	return startStdio(s)
}

// ---- shared tool plumbing -------------------------------------------------

// rpc is a transport that can issue JSON-RPC requests/notifications.
type rpc interface {
	call(method string, params any) (json.RawMessage, error)
	notify(method string, params any)
	close()
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

func listTools(c rpc) ([]mcpTool, error) {
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

func callTool(c rpc, name string, args map[string]any) string {
	raw, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var out struct {
		Content []struct {
			Type, Text string
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

func adapt(server string, c rpc, m mcpTool) core.Tool {
	params := m.InputSchema
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	cc, real := c, m.Name
	return core.Tool{
		Name:        sanitize(server) + "__" + sanitize(m.Name),
		Description: "[" + server + "] " + m.Description,
		Parameters:  params,
		Func:        func(_ context.Context, args map[string]any) string { return callTool(cc, real, args) },
	}
}

// parseRPC extracts the JSON-RPC response with the given id from a body that is
// either a plain JSON object or an SSE stream of `data:` events.
func parseRPC(data []byte, contentType string, id int) (json.RawMessage, error) {
	var bodies [][]byte
	if strings.Contains(contentType, "text/event-stream") || bytes.Contains(data, []byte("data:")) {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if line = bytes.TrimSpace(line); bytes.HasPrefix(line, []byte("data:")) {
				bodies = append(bodies, bytes.TrimSpace(line[5:]))
			}
		}
	} else {
		bodies = [][]byte{bytes.TrimSpace(data)}
	}
	for _, b := range bodies {
		var resp rpcResp
		if json.Unmarshal(b, &resp) != nil || resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("no matching JSON-RPC response")
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

// ---- stdio transport ------------------------------------------------------

type stdioClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	mu     sync.Mutex
	nextID int
}

func startStdio(s Server) (rpc, error) {
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
	c := &stdioClient{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}
	if _, err := c.call("initialize", initParams); err != nil {
		c.close()
		return nil, err
	}
	c.notify("notifications/initialized", map[string]any{})
	return c, nil
}

func (c *stdioClient) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
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
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *stdioClient) notify(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	_, _ = c.stdin.Write(append(b, '\n'))
}

func (c *stdioClient) close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

// ---- HTTP transport (Streamable HTTP / SSE) -------------------------------

type httpClient struct {
	url     string
	headers map[string]string
	hc      *http.Client
	mu      sync.Mutex
	nextID  int
	session string
}

func startHTTP(s Server) (rpc, error) {
	c := &httpClient{url: s.URL, headers: s.Headers, hc: &http.Client{Timeout: 120 * time.Second}}
	if _, err := c.call("initialize", initParams); err != nil {
		return nil, err
	}
	c.notify("notifications/initialized", map[string]any{})
	return c, nil
}

func (c *httpClient) post(payload map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return c.hc.Do(req)
}

func (c *httpClient) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	resp, err := c.post(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.session = sid
	}
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return parseRPC(data, resp.Header.Get("Content-Type"), id)
}

func (c *httpClient) notify(method string, params any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if resp, err := c.post(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err == nil {
		resp.Body.Close()
	}
}

func (c *httpClient) close() {}
