// Package agent implements the generic agent loop.
//
// One Agent = one (provider, model, system prompt, tool set). The same loop
// drives the orchestrator and the executors. A Session keeps conversation state
// across turns; SendCtx runs one turn under a context (so it can be aborted or
// time-limited). The loop also:
//   - detects stuck executors (exact and windowed tool-call repetition),
//   - self-compacts its own history when it grows past a token budget,
//   - can summarize its progress for a resume-with-summary handoff.
//
// Every turn returns a RunResult whose Status is the supervision signal:
// completed | max_turns | looping | context_exhausted | timeout | aborted | error.
package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/TedHaley/cheep/internal/core"
)

type RunResult struct {
	Status       string
	Output       string
	Summary      string // handoff summary, set when a run is cut short and summarized
	Stuck        string // the repeated tool call, when Status=="looping" (for a smarter re-prompt)
	Turns        int
	InputTokens  int
	OutputTokens int
}

type Agent struct {
	Name          string
	Provider      core.Provider
	Model         string
	System        string
	Tools         []core.Tool
	MaxTurns      int
	TokenBudget   int // stop a run past this many cumulative input tokens (0 = off)
	CompactBudget int // self-compact history past this many estimated tokens (0 = off)
	OnEvent       core.EventFunc
	LoopWindow    int

	// CompactNote, when set, receives every compaction summary so squeezed-out
	// context can be persisted as durable memory (e.g. the run notes).
	CompactNote func(summary string)

	byName map[string]core.Tool
}

func New(name string, provider core.Provider, model, system string, tools []core.Tool,
	maxTurns, tokenBudget int, onEvent core.EventFunc) *Agent {
	byName := make(map[string]core.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	if onEvent == nil {
		onEvent = func(core.Event) {}
	}
	return &Agent{
		Name: name, Provider: provider, Model: model, System: system, Tools: tools,
		MaxTurns: maxTurns, TokenBudget: tokenBudget, OnEvent: onEvent, LoopWindow: 3,
		byName: byName,
	}
}

func (a *Agent) emit(e core.Event) {
	e.Agent = a.Name
	a.OnEvent(e)
}

// Run executes a single task in a fresh conversation.
func (a *Agent) Run(task string) RunResult { return a.NewSession().Send(task) }

// Session is a conversation with the agent that persists across turns.
type Session struct {
	a        *Agent
	messages []core.Message
}

func (a *Agent) NewSession() *Session { return &Session{a: a} }

// Resume continues a conversation from prior history (e.g. after a mode switch
// that rebuilt the agent with a different tool set).
func (a *Agent) Resume(history []core.Message) *Session {
	return &Session{a: a, messages: append([]core.Message{}, history...)}
}

// History returns the conversation so far.
func (s *Session) History() []core.Message { return s.messages }

// Send runs a turn with no deadline.
func (s *Session) Send(userText string) RunResult {
	return s.SendCtx(context.Background(), userText)
}

// SendCtx appends the user message and runs the loop until the agent stops
// calling tools or hits a limit. The context can cancel/time-limit the run.
func (s *Session) SendCtx(ctx context.Context, userText string) RunResult {
	a := s.a
	s.messages = append(s.messages, core.Message{Role: "user", Text: userText})
	inTok, outTok := 0, 0
	emptyNudges := 0         // times we re-prompted an empty turn (small models stop after a tool)
	overflowRetried := false // one compact-and-retry per Send when the window overflows
	var recent []string      // tool-call signatures, for stuck detection

	// MaxTurns <= 0 means unlimited (loop mode) — bounded instead by loop
	// detection, the budget cap, context compaction, and user cancellation.
	for turn := 1; a.MaxTurns <= 0 || turn <= a.MaxTurns; turn++ {
		if st := ctxStatus(ctx); st != "" {
			return s.result(st, inTok, outTok, turn-1)
		}
		s.maybeCompact(ctx)

		res, err := a.Provider.Complete(ctx, a.Model, a.System, s.messages, a.Tools)
		if err != nil {
			if st := ctxStatus(ctx); st != "" {
				return s.result(st, inTok, outTok, turn)
			}
			// The model's real context window can be far smaller than
			// CompactBudget (local models): on an overflow rejection, compact
			// aggressively (chunked, so the summarizer can't overflow either)
			// and retry once.
			if !overflowRetried && isContextError(err) {
				overflowRetried = true
				a.emit(core.Event{Type: "status", Status: "context window overflow — compacting and retrying"})
				if s.compact(ctx, 4, true) {
					continue
				}
			}
			a.emit(core.Event{Type: "error", Text: err.Error()})
			return RunResult{Status: "error", Output: "provider error: " + err.Error(),
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
		}
		inTok += res.InputTokens
		outTok += res.OutputTokens
		msg := res.Message
		s.messages = append(s.messages, msg)
		a.emit(core.Event{Type: "progress", Turn: turn, Tokens: inTok, Ctx: EstTokens(s.messages)})
		a.emit(core.Event{Type: "usage", Model: a.Model, InTok: res.InputTokens, OutTok: res.OutputTokens})
		if msg.Text != "" {
			a.emit(core.Event{Type: "text", Text: msg.Text})
		}

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Text) == "" {
				// The model ended its turn with no answer. If it just ran a tool,
				// nudge it once to actually reply (so the user isn't left blank);
				// otherwise surface a visible note rather than nothing.
				prevRole := ""
				if len(s.messages) >= 2 {
					prevRole = s.messages[len(s.messages)-2].Role
				}
				if emptyNudges < 1 && prevRole == "tool" {
					emptyNudges++
					s.messages = s.messages[:len(s.messages)-1] // drop the empty turn
					s.messages = append(s.messages, core.Message{Role: "user",
						Text: "You ran a tool but didn't reply. Answer my request in plain text now — summarize what you found. Do not call any tools."})
					continue
				}
				a.emit(core.Event{Type: "text", Text: "_(no response — the model ended its turn without replying; try rephrasing)_"})
			}
			return RunResult{Status: "completed", Output: msg.Text,
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
		}

		if a.TokenBudget > 0 && inTok > a.TokenBudget {
			a.emit(core.Event{Type: "status", Status: "context_exhausted"})
			return RunResult{Status: "context_exhausted", Output: msg.Text,
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
		}

		for _, tc := range msg.ToolCalls {
			recent = append(recent, tc.Name+"|"+canonicalArgs(tc.Arguments))
			a.emit(core.Event{Type: "tool_call", Tool: tc.Name, Args: tc.Arguments})
			var resultText string
			if t, ok := a.byName[tc.Name]; ok {
				resultText = t.Func(ctx, tc.Arguments)
			} else {
				resultText = "ERROR: unknown tool " + tc.Name
			}
			a.emit(core.Event{Type: "tool_result", Tool: tc.Name, Result: resultText})
			s.messages = append(s.messages, core.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Text: resultText,
			})
		}

		if status := detectStuck(recent, a.LoopWindow); status != "" {
			a.emit(core.Event{Type: "status", Status: status})
			rr := RunResult{Status: status, Output: msg.Text,
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
			if status == "looping" && len(recent) > 0 {
				rr.Stuck = recent[len(recent)-1] // the repeated call, for a smarter re-prompt
			}
			return rr
		}
	}

	a.emit(core.Event{Type: "status", Status: "max_turns"})
	return s.result("max_turns", inTok, outTok, a.MaxTurns)
}

// Summarize asks the model for a concise handoff of progress so far.
func (s *Session) Summarize(ctx context.Context) string {
	return summarize(ctx, s.a.Provider, s.a.Model, s.messages,
		"Summarize concisely what has been accomplished so far and exactly what remains to be done.")
}

func (s *Session) result(status string, inTok, outTok, turns int) RunResult {
	return RunResult{Status: status, Output: lastText(s.messages),
		Turns: turns, InputTokens: inTok, OutputTokens: outTok}
}

// maybeCompact summarizes and replaces the older part of the history when the
// estimated context exceeds CompactBudget, keeping recent turns intact.
func (s *Session) maybeCompact(ctx context.Context) {
	if s.a.CompactBudget <= 0 || EstTokens(s.messages) <= s.a.CompactBudget {
		return
	}
	s.compact(ctx, 8, false)
}

const compactInstruction = "Summarize the earlier part of this conversation into a concise brief, preserving decisions, facts, file changes, executor results, and open tasks."

// compact summarizes and replaces everything but the last keepRecent messages.
// chunked summarizes the prefix in bounded pieces (for use right after the
// provider rejected the full history — the summarizer must fit too). The
// summary replaces the prefix in-context and is handed to CompactNote so the
// squeezed-out memory survives on disk. Reports whether it compacted.
func (s *Session) compact(ctx context.Context, keepRecent int, chunked bool) bool {
	a := s.a
	if len(s.messages) <= keepRecent+2 {
		return false
	}
	// Cut at a turn boundary: an assistant message whose predecessor is a tool
	// result or user message, so prefix and suffix are each self-contained.
	cut := -1
	for j := len(s.messages) - keepRecent; j >= 1; j-- {
		if s.messages[j].Role == "assistant" {
			if pr := s.messages[j-1].Role; pr == "tool" || pr == "user" {
				cut = j
				break
			}
		}
	}
	if cut < 1 {
		return false
	}
	var summary string
	if chunked {
		summary = summarizeChunked(ctx, a.Provider, a.Model, s.messages[:cut])
	} else {
		summary = summarize(ctx, a.Provider, a.Model, s.messages[:cut], compactInstruction)
	}
	if summary == "" {
		return false
	}
	a.emit(core.Event{Type: "status", Status: "compacted context"})
	if a.CompactNote != nil {
		a.CompactNote(summary)
	}
	s.messages = append([]core.Message{{Role: "user", Text: "[Earlier conversation summary]\n" + summary}}, s.messages[cut:]...)
	return true
}

// summarizeChunked summarizes msgs in pieces small enough that each summarize
// call is far below the history size that just overflowed the window.
func summarizeChunked(ctx context.Context, p core.Provider, model string, msgs []core.Message) string {
	limit := EstTokens(msgs)/4 + 2000
	var parts []string
	var chunk []core.Message
	flush := func() {
		if len(chunk) == 0 {
			return
		}
		if chunk[0].Role != "user" { // keep role alternation valid for strict providers
			chunk = append([]core.Message{{Role: "user", Text: "[conversation fragment continues]"}}, chunk...)
		}
		if s := summarize(ctx, p, model, chunk, compactInstruction); s != "" {
			parts = append(parts, s)
		}
		chunk = nil
	}
	for _, m := range msgs {
		chunk = append(chunk, m)
		if EstTokens(chunk) >= limit {
			flush()
		}
	}
	flush()
	return strings.Join(parts, "\n\n")
}

// isContextError reports whether a provider rejection looks like a context
// window overflow (wordings vary: OpenAI, Anthropic, LM Studio, llama.cpp).
func isContextError(err error) bool {
	e := strings.ToLower(err.Error())
	for _, sig := range []string{
		"context length", "context window", "maximum context", "context size",
		"prompt is too long", "too many tokens", "exceeds the model",
	} {
		if strings.Contains(e, sig) {
			return true
		}
	}
	return strings.Contains(e, "context") && strings.Contains(e, "exceed")
}

// summarize runs one tool-less completion to summarize a conversation. The
// instruction goes in the system prompt to avoid breaking role alternation.
func summarize(ctx context.Context, p core.Provider, model string, msgs []core.Message, instruction string) string {
	conv := sanitize(msgs)
	if len(conv) == 0 {
		return ""
	}
	sys := "You are a concise summarizer. " + instruction + " Output only the summary text."
	turn, err := p.Complete(ctx, model, sys, conv, nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(turn.Message.Text)
}

// sanitize returns a valid conversation: it drops a trailing assistant turn that
// has unanswered tool calls, and any leading non-user messages.
func sanitize(msgs []core.Message) []core.Message {
	out := append([]core.Message{}, msgs...)
	for len(out) > 0 {
		last := out[len(out)-1]
		if last.Role == "assistant" && len(last.ToolCalls) > 0 {
			out = out[:len(out)-1]
			continue
		}
		break
	}
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out
}

// detectStuck flags looping: the same call repeated LoopWindow times in a row,
// or the same call appearing 4+ times within the last 8 calls.
func detectStuck(recent []string, window int) string {
	n := len(recent)
	if n >= window {
		same := true
		for i := n - window; i < n-1; i++ {
			if recent[i] != recent[n-1] {
				same = false
				break
			}
		}
		if same {
			return "looping"
		}
	}
	const w, thresh = 8, 4
	if n >= thresh {
		start := n - w
		if start < 0 {
			start = 0
		}
		counts := map[string]int{}
		for _, sig := range recent[start:] {
			counts[sig]++
			if counts[sig] >= thresh {
				return "looping"
			}
		}
	}
	return ""
}

func ctxStatus(ctx context.Context) string {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return "timeout"
	case context.Canceled:
		return "aborted"
	}
	return ""
}

func lastText(msgs []core.Message) string {
	if n := len(msgs); n > 0 {
		return msgs[n-1].Text
	}
	return ""
}

// EstTokens estimates the token size of a conversation (chars/4 heuristic) —
// the same measure that drives self-compaction, exported for the context bar.
func EstTokens(msgs []core.Message) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Text)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(canonicalArgs(tc.Arguments))
		}
	}
	return chars / 4
}

// canonicalArgs serializes args deterministically (Go sorts map keys in JSON).
func canonicalArgs(args map[string]any) string {
	b, _ := json.Marshal(args)
	return string(b)
}
