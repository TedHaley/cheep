package agent

import (
	"context"
	"testing"

	"github.com/TedHaley/cheep/internal/core"
)

// scriptProvider returns a fixed sequence of turns, ignoring its inputs.
type scriptProvider struct {
	turns []core.Turn
	i     int
}

func (s *scriptProvider) Complete(_ context.Context, _, _ string, _ []core.Message, _ []core.Tool) (core.Turn, error) {
	t := s.turns[s.i]
	s.i++
	return t, nil
}

func toolCallTurn(name string, args map[string]any) core.Turn {
	return core.Turn{Message: core.Message{
		Role:      "assistant",
		ToolCalls: []core.ToolCall{{ID: "1", Name: name, Arguments: args}},
	}}
}

func TestRunCompletesAfterToolCall(t *testing.T) {
	called := false
	tools := []core.Tool{{
		Name: "ping",
		Func: func(context.Context, map[string]any) string { called = true; return "pong" },
	}}
	p := &scriptProvider{turns: []core.Turn{
		{Message: core.Message{Role: "assistant", ToolCalls: []core.ToolCall{
			{ID: "1", Name: "ping", Arguments: map[string]any{}}}}, InputTokens: 10, OutputTokens: 5},
		{Message: core.Message{Role: "assistant", Text: "done"}, InputTokens: 8, OutputTokens: 2},
	}}

	r := New("t", p, "m", "sys", tools, 10, 0, nil).Run("task")

	if r.Status != "completed" {
		t.Fatalf("status = %q, want completed", r.Status)
	}
	if !called {
		t.Fatal("tool was not invoked")
	}
	if r.Output != "done" {
		t.Fatalf("output = %q, want done", r.Output)
	}
	if r.InputTokens != 18 || r.OutputTokens != 7 {
		t.Fatalf("tokens in=%d out=%d, want 18/7", r.InputTokens, r.OutputTokens)
	}
}

func TestLoopDetection(t *testing.T) {
	tools := []core.Tool{{Name: "spin", Func: func(context.Context, map[string]any) string { return "x" }}}
	loop := toolCallTurn("spin", map[string]any{"a": 1.0})
	p := &scriptProvider{turns: []core.Turn{loop, loop, loop, loop}}

	r := New("t", p, "m", "sys", tools, 10, 0, nil).Run("task")

	if r.Status != "looping" {
		t.Fatalf("status = %q, want looping", r.Status)
	}
}

func TestMaxTurns(t *testing.T) {
	// Distinct args each turn so loop detection does not fire first.
	tools := []core.Tool{{Name: "spin", Func: func(context.Context, map[string]any) string { return "x" }}}
	turns := []core.Turn{
		toolCallTurn("spin", map[string]any{"i": 1.0}),
		toolCallTurn("spin", map[string]any{"i": 2.0}),
		toolCallTurn("spin", map[string]any{"i": 3.0}),
	}
	p := &scriptProvider{turns: turns}

	r := New("t", p, "m", "sys", tools, 3, 0, nil).Run("task")

	if r.Status != "max_turns" {
		t.Fatalf("status = %q, want max_turns", r.Status)
	}
	if r.Turns != 3 {
		t.Fatalf("turns = %d, want 3", r.Turns)
	}
}

func TestDetectStuckConsecutive(t *testing.T) {
	if detectStuck([]string{"a", "a", "a"}, 3) != "looping" {
		t.Fatal("consecutive repeat not detected")
	}
}

func TestDetectStuckWindowed(t *testing.T) {
	// "x" appears 4 times within the window, not consecutively.
	if detectStuck([]string{"x", "y", "x", "z", "x", "w", "x"}, 3) != "looping" {
		t.Fatal("windowed repeat not detected")
	}
	if detectStuck([]string{"a", "b", "c", "d"}, 3) != "" {
		t.Fatal("false positive on distinct calls")
	}
}

func TestSendAbortedOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// nil turns => Complete would panic if called; cancellation must prevent that.
	r := New("t", &scriptProvider{}, "m", "sys", nil, 5, 0, nil).NewSession().SendCtx(ctx, "hi")
	if r.Status != "aborted" {
		t.Fatalf("status = %q, want aborted", r.Status)
	}
}

func TestSanitizeDropsOrphanAssistant(t *testing.T) {
	msgs := []core.Message{
		{Role: "user", Text: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "1", Name: "x"}}},
	}
	out := sanitize(msgs)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("expected just the user message, got %+v", out)
	}
}

func TestContextExhausted(t *testing.T) {
	tools := []core.Tool{{Name: "spin", Func: func(context.Context, map[string]any) string { return "x" }}}
	turn := toolCallTurn("spin", map[string]any{"i": 1.0})
	turn.InputTokens = 200
	p := &scriptProvider{turns: []core.Turn{turn, turn}}

	r := New("t", p, "m", "sys", tools, 10, 100, nil).Run("task") // budget 100 < 200

	if r.Status != "context_exhausted" {
		t.Fatalf("status = %q, want context_exhausted", r.Status)
	}
}

// TestUnlimitedTurns: MaxTurns<=0 (loop mode) must run until the model stops,
// not cap out and not spin forever on a normal completion.
func TestUnlimitedTurns(t *testing.T) {
	tools := []core.Tool{{Name: "step", Func: func(context.Context, map[string]any) string { return "ok" }}}
	// three tool-call turns (varied args so loop detection doesn't trip), then text
	p := &scriptProvider{turns: []core.Turn{
		toolCallTurn("step", map[string]any{"n": 1.0}),
		toolCallTurn("step", map[string]any{"n": 2.0}),
		toolCallTurn("step", map[string]any{"n": 3.0}),
		{Message: core.Message{Role: "assistant", Text: "done"}},
	}}
	r := New("t", p, "m", "sys", tools, 0 /* unlimited */, 0, nil).Run("go")
	if r.Status != "completed" || r.Output != "done" {
		t.Fatalf("unlimited-turns run = %q/%q, want completed/done", r.Status, r.Output)
	}
	if r.Turns != 4 {
		t.Errorf("turns = %d, want 4", r.Turns)
	}
}
