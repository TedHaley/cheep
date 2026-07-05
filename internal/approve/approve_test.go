package approve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

func writeTool(hits *int) core.Tool {
	return core.Tool{
		Name: "write_file",
		Func: func(context.Context, map[string]any) string { *hits++; return "ok" },
	}
}

func bashTool(hits *int) core.Tool {
	return core.Tool{
		Name: "run_bash",
		Func: func(context.Context, map[string]any) string { *hits++; return "ok" },
	}
}

// respond answers every request on g with d.
func respond(t *testing.T, g *Gate, d Decision) func() {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case r := <-g.Requests:
				r.Resp <- d
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

func TestModes(t *testing.T) {
	args := map[string]any{"path": "a.txt", "content": "x", "command": "ls"}

	t.Run("yolo gates nothing", func(t *testing.T) {
		g := New(ModeYolo)
		hits := 0
		tools := g.Wrap([]core.Tool{writeTool(&hits), bashTool(&hits)}, true, t.TempDir())
		for _, tl := range tools {
			if out := tl.Func(context.Background(), args); out != "ok" {
				t.Fatal(out)
			}
		}
		if hits != 2 {
			t.Fatal("tools not executed")
		}
	})

	t.Run("auto gates shared writes only", func(t *testing.T) {
		g := New(ModeAuto)
		stop := respond(t, g, Allow)
		defer stop()
		hits := 0
		tools := g.Wrap([]core.Tool{writeTool(&hits), bashTool(&hits)}, true, t.TempDir())
		// bash passes without approval even with no responder needed; write asks (responder allows).
		for _, tl := range tools {
			if out := tl.Func(context.Background(), args); out != "ok" {
				t.Fatal(out)
			}
		}
		if hits != 2 {
			t.Fatal("allow must execute")
		}
	})

	t.Run("approve gates bash too and deny blocks", func(t *testing.T) {
		g := New(ModeApprove)
		stop := respond(t, g, Deny)
		defer stop()
		hits := 0
		tools := g.Wrap([]core.Tool{bashTool(&hits)}, true, t.TempDir())
		out := tools[0].Func(context.Background(), args)
		if !strings.HasPrefix(out, "ERROR:") || hits != 0 {
			t.Fatalf("deny must block: %q hits=%d", out, hits)
		}
	})

	t.Run("worktree tools never gated", func(t *testing.T) {
		g := New(ModeApprove)
		hits := 0
		tools := g.Wrap([]core.Tool{writeTool(&hits)}, false, t.TempDir())
		if out := tools[0].Func(context.Background(), args); out != "ok" || hits != 1 {
			t.Fatal("isolated tool was gated")
		}
	})
}

func TestAllowSessionStopsAsking(t *testing.T) {
	g := New(ModeApprove)
	asked := 0
	go func() {
		for r := range g.Requests {
			asked++
			r.Resp <- AllowSession
		}
	}()
	hits := 0
	tools := g.Wrap([]core.Tool{bashTool(&hits)}, true, t.TempDir())
	args := map[string]any{"command": "ls"}
	tools[0].Func(context.Background(), args)
	tools[0].Func(context.Background(), args)
	if asked != 1 || hits != 2 {
		t.Fatalf("asked=%d hits=%d, want 1/2", asked, hits)
	}
}

func TestCancelUnblocks(t *testing.T) {
	g := New(ModeApprove)
	hits := 0
	tools := g.Wrap([]core.Tool{bashTool(&hits)}, true, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan string, 1)
	go func() { res <- tools[0].Func(ctx, map[string]any{"command": "ls"}) }()
	time.Sleep(20 * time.Millisecond) // let it block on the (undrained) request
	cancel()
	select {
	case out := <-res:
		if !strings.Contains(out, "cancelled") || hits != 0 {
			t.Fatalf("%q hits=%d", out, hits)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gated call deadlocked after cancel")
	}
}

func TestDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"
	new := "a\nb\nc\nd\ne\nX\ng\nh\ni\nj\nk\n"
	d := Diff(old, new)
	if !strings.Contains(d, "- f") || !strings.Contains(d, "+ X") {
		t.Fatalf("missing change lines:\n%s", d)
	}
	if !strings.Contains(d, "···") {
		t.Fatalf("far context not collapsed:\n%s", d)
	}
	if strings.Contains(d, "  a\n") {
		t.Fatalf("line 'a' is outside context and should be collapsed:\n%s", d)
	}
	if Diff("same", "same") != "(no change)" {
		t.Fatal("identical inputs")
	}
	if d := Diff("", "new file\n"); !strings.Contains(d, "+ new file") {
		t.Fatalf("new file diff: %s", d)
	}
}
