package piext

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TedHaley/cheep/internal/mcp"
)

const testExt = `export default function (pi) {
  pi.on("session_start", () => {}); // must be skipped, not fatal
  pi.registerTool({
    name: "add_numbers",
    description: "Add two numbers",
    parameters: { type: "object", properties: { a: { type: "number" }, b: { type: "number" } }, required: ["a","b"] },
    async execute(_id, p) {
      return { content: [{ type: "text", text: String(p.a + p.b) }] };
    },
  });
}
`

// TestBridgeThroughMCPClient runs a pi-style extension through the embedded
// Node bridge via cheep's real MCP client — the exact production path.
func TestBridgeThroughMCPClient(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	t.Setenv("CHEEP_HOME", t.TempDir())

	extPath := filepath.Join(t.TempDir(), "ext.mjs")
	if err := os.WriteFile(extPath, []byte(testExt), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := Server([]string{extPath})
	if err != nil {
		t.Fatal(err)
	}
	if srv == nil {
		t.Fatal("Server returned nil for a non-empty extension list")
	}

	tools, sess := mcp.Start(map[string]mcp.Server{"pi": *srv}, nil)
	defer sess.Close()
	if len(tools.Orchestrator) != 1 || len(tools.Executor) != 1 {
		t.Fatalf("want 1 tool per role, got %d/%d", len(tools.Orchestrator), len(tools.Executor))
	}
	tool := tools.Orchestrator[0]
	if tool.Name != "pi__add_numbers" {
		t.Fatalf("unexpected tool name %q", tool.Name)
	}
	got := tool.Func(context.Background(), map[string]any{"a": float64(2), "b": float64(40)})
	if got != "42" {
		t.Fatalf("add_numbers = %q, want 42", got)
	}
}

func TestServerNilWithoutExtensions(t *testing.T) {
	s, err := Server(nil)
	if err != nil || s != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", s, err)
	}
}
