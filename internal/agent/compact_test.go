package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TedHaley/cheep/internal/core"
)

// seqProvider returns scripted (turn, err) pairs in order.
type seqProvider struct {
	steps []struct {
		turn core.Turn
		err  error
	}
	i     int
	calls []int // message counts per call, for assertions
}

func (s *seqProvider) Complete(_ context.Context, _, _ string, msgs []core.Message, _ []core.Tool) (core.Turn, error) {
	s.calls = append(s.calls, len(msgs))
	st := s.steps[s.i]
	if s.i < len(s.steps)-1 {
		s.i++
	}
	return st.turn, st.err
}

func text(t string) core.Turn {
	return core.Turn{Message: core.Message{Role: "assistant", Text: t}}
}

// longHistory builds an alternating conversation big enough to compact.
func longHistory(turns int) []core.Message {
	var msgs []core.Message
	filler := strings.Repeat("x", 400)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			core.Message{Role: "user", Text: "step " + filler},
			core.Message{Role: "assistant", Text: "did step " + filler})
	}
	return msgs
}

func TestOverflowCompactsAndRetries(t *testing.T) {
	p := &seqProvider{}
	add := func(turn core.Turn, err error) {
		p.steps = append(p.steps, struct {
			turn core.Turn
			err  error
		}{turn, err})
	}
	add(core.Turn{}, errors.New("openai 400: this model's maximum context length is 4096 tokens"))
	add(text("chunk summary"), nil) // summarize call(s) during chunked compaction
	add(text("recovered fine"), nil)

	var noted string
	a := New("t", p, "m", "sys", nil, 10, 0, nil)
	a.CompactNote = func(s string) { noted = s }
	sess := a.Resume(longHistory(10))

	r := sess.Send("finish up")
	if r.Status != "completed" || r.Output != "recovered fine" {
		t.Fatalf("want recovery after overflow, got %q / %q", r.Status, r.Output)
	}
	if noted == "" || !strings.Contains(noted, "chunk summary") {
		t.Fatalf("compaction summary was not persisted via CompactNote: %q", noted)
	}
	// the retried request must be materially smaller than the failing one
	first, last := p.calls[0], p.calls[len(p.calls)-1]
	if last >= first {
		t.Fatalf("retry did not shrink the conversation: first=%d last=%d", first, last)
	}
}

func TestBudgetCompactionKeepsRecentAndNotes(t *testing.T) {
	p := &seqProvider{}
	p.steps = append(p.steps, struct {
		turn core.Turn
		err  error
	}{text("summary of the past"), nil}) // summarize
	p.steps = append(p.steps, struct {
		turn core.Turn
		err  error
	}{text("done"), nil}) // the actual turn

	var noted string
	a := New("t", p, "m", "sys", nil, 10, 0, nil)
	a.CompactBudget = 100 // tiny: force compaction immediately
	a.CompactNote = func(s string) { noted = s }
	sess := a.Resume(longHistory(12))

	r := sess.Send("continue")
	if r.Status != "completed" {
		t.Fatalf("status %q", r.Status)
	}
	if noted != "summary of the past" {
		t.Fatalf("CompactNote got %q", noted)
	}
	msgs := sess.History()
	if !strings.Contains(msgs[0].Text, "[Earlier conversation summary]") {
		t.Fatalf("history not compacted; first message: %.60q", msgs[0].Text)
	}
	if EstTokens(msgs) >= EstTokens(longHistory(12)) {
		t.Fatal("compaction did not shrink the history")
	}
}

func TestIsContextError(t *testing.T) {
	yes := []string{
		"openai 400: this model's maximum context length is 4096 tokens",
		"anthropic 400: prompt is too long: 210000 tokens > 200000 maximum",
		"openai 400: the number of tokens to keep from the initial prompt is greater than the context length",
		"openai 400: trying to submit a prompt that exceeds the model limit",
		"context window exceeded",
	}
	no := []string{
		"openai 401: invalid api key",
		"anthropic 529: overloaded",
		"connection refused",
	}
	for _, s := range yes {
		if !isContextError(errors.New(s)) {
			t.Errorf("should match: %s", s)
		}
	}
	for _, s := range no {
		if isContextError(errors.New(s)) {
			t.Errorf("should NOT match: %s", s)
		}
	}
}
