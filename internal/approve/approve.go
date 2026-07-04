// Package approve gates risky tool calls behind user approval, with a diff
// preview for file writes. Three modes, per Goose's legible model:
//
//	yolo    — nothing is gated.
//	auto    — file writes to the SHARED workspace ask first; everything in an
//	          isolated worktree is ungated (the pre-merge validation pipeline
//	          and the merge itself are that work's gate). This is the default.
//	approve — shared writes AND shared shell commands ask first.
//
// A gated tool call blocks its agent goroutine on a Request sent to the UI;
// the Bubble Tea loop stays live because agent runs happen inside tea.Cmd
// goroutines. Every wait also selects on ctx.Done so cancelling a run (esc)
// never deadlocks an agent.
package approve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/TedHaley/cheep/internal/core"
)

// Mode is the approval strictness.
type Mode string

const (
	ModeYolo    Mode = "yolo"
	ModeAuto    Mode = "auto"
	ModeApprove Mode = "approve"
)

// ParseMode returns the mode for a name, or false if unknown.
func ParseMode(s string) (Mode, bool) {
	switch Mode(s) {
	case ModeYolo, ModeAuto, ModeApprove:
		return Mode(s), true
	}
	return "", false
}

// Decision is the user's answer to a Request.
type Decision int

const (
	Deny Decision = iota
	Allow
	AllowSession // allow, and stop asking for this tool this session
)

// Request is one pending approval, shown by the UI.
type Request struct {
	Agent string         // which agent wants to act
	Tool  string         // "write_file" | "run_bash"
	Path  string         // file path (write_file)
	Cmd   string         // command (run_bash)
	Diff  string         // rendered diff preview (write_file)
	Args  map[string]any // raw tool args
	Resp  chan Decision  // buffered(1); the UI sends exactly one Decision
}

// Gate wraps tools with approval checks. The zero value is unusable; use New.
type Gate struct {
	mode     atomic.Value // Mode
	Requests chan Request
	allowed  sync.Map // tool name -> true (session allows)
}

// New returns a Gate starting in mode (default auto for "").
func New(m Mode) *Gate {
	if m == "" {
		m = ModeAuto
	}
	g := &Gate{Requests: make(chan Request, 16)}
	g.mode.Store(m)
	return g
}

// SetMode switches strictness; takes effect on the next tool call.
func (g *Gate) SetMode(m Mode) { g.mode.Store(m) }

// Mode returns the current strictness.
func (g *Gate) Mode() Mode { return g.mode.Load().(Mode) }

// Ask blocks on a bespoke approval request outside the tool path (e.g. a
// branch merge in no-mistakes mode) and returns the user's decision. Fails
// closed: a nil Gate, a cancelled run, or an absent approver all deny —
// nothing lands without an explicit yes.
func (g *Gate) Ask(ctx context.Context, req Request) Decision {
	if g == nil {
		return Deny
	}
	req.Resp = make(chan Decision, 1)
	select {
	case g.Requests <- req:
	case <-ctx.Done():
		return Deny
	}
	select {
	case d := <-req.Resp:
		return d
	case <-ctx.Done():
		return Deny
	}
}

// Wrap returns tools with gating applied. shared marks tools operating on the
// user's real workspace (as opposed to an isolated worktree); only shared
// tools are ever gated. workdir resolves relative paths for diff previews. A
// nil Gate is a no-op.
func (g *Gate) Wrap(tools []core.Tool, shared bool, workdir string) []core.Tool {
	if g == nil || !shared {
		return tools
	}
	out := make([]core.Tool, len(tools))
	for i, t := range tools {
		out[i] = t
		switch t.Name {
		case "write_file", "run_bash":
			out[i].Func = g.gated(t, workdir)
		}
	}
	return out
}

func (g *Gate) gated(t core.Tool, workdir string) func(context.Context, map[string]any) string {
	inner := t.Func
	agentLabel := "agent"
	return func(ctx context.Context, args map[string]any) string {
		mode := g.Mode()
		gate := mode == ModeApprove || (mode == ModeAuto && t.Name == "write_file")
		if !gate {
			return inner(ctx, args)
		}
		if _, ok := g.allowed.Load(t.Name); ok {
			return inner(ctx, args)
		}

		req := Request{Agent: agentLabel, Tool: t.Name, Args: args, Resp: make(chan Decision, 1)}
		switch t.Name {
		case "write_file":
			req.Path, _ = args["path"].(string)
			content, _ := args["content"].(string)
			old := ""
			if b, err := os.ReadFile(filepath.Join(workdir, req.Path)); err == nil {
				old = string(b)
			}
			req.Diff = Diff(old, content)
		case "run_bash":
			req.Cmd, _ = args["command"].(string)
		}

		select {
		case g.Requests <- req:
		case <-ctx.Done():
			return "ERROR: run cancelled while awaiting approval"
		}
		select {
		case d := <-req.Resp:
			switch d {
			case Allow:
				return inner(ctx, args)
			case AllowSession:
				g.allowed.Store(t.Name, true)
				return inner(ctx, args)
			default:
				return fmt.Sprintf("ERROR: the user declined this %s. Do not retry it verbatim — "+
					"explain what you wanted to do and ask how to proceed.", t.Name)
			}
		case <-ctx.Done():
			return "ERROR: run cancelled while awaiting approval"
		}
	}
}
