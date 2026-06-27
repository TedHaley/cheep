// Package agent implements the generic agent loop.
//
// One Agent = one (provider, model, system prompt, tool set). The same loop
// drives the orchestrator and the executors. A Session keeps conversation state
// across turns (for interactive chat); Run is the one-shot convenience.
//
// Every turn returns a RunResult whose Status is the seed of the supervision
// story: anything other than "completed" is a signal the caller can act on
// (re-scope, retry, escalate).
package agent

import (
	"encoding/json"

	"github.com/TedHaley/cheep/internal/core"
)

type RunResult struct {
	Status       string // completed | max_turns | looping | context_exhausted | error
	Output       string
	Turns        int
	InputTokens  int
	OutputTokens int
}

type Agent struct {
	Name        string
	Provider    core.Provider
	Model       string
	System      string
	Tools       []core.Tool
	MaxTurns    int
	TokenBudget int
	OnEvent     core.EventFunc
	LoopWindow  int

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
func (a *Agent) Run(task string) RunResult {
	return a.NewSession().Send(task)
}

// Session is a conversation with the agent that persists across turns.
type Session struct {
	a        *Agent
	messages []core.Message
}

func (a *Agent) NewSession() *Session { return &Session{a: a} }

// Send appends the user message, runs the loop until the agent stops calling
// tools (or hits a limit), and keeps the resulting history for the next turn.
func (s *Session) Send(userText string) RunResult {
	a := s.a
	s.messages = append(s.messages, core.Message{Role: "user", Text: userText})
	inTok, outTok := 0, 0
	var recent []string // tool-call signatures, for loop detection

	for turn := 1; turn <= a.MaxTurns; turn++ {
		res, err := a.Provider.Complete(a.Model, a.System, s.messages, a.Tools)
		if err != nil {
			a.emit(core.Event{Type: "error", Text: err.Error()})
			return RunResult{Status: "error", Output: "provider error: " + err.Error(),
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
		}
		inTok += res.InputTokens
		outTok += res.OutputTokens
		msg := res.Message
		s.messages = append(s.messages, msg)
		if msg.Text != "" {
			a.emit(core.Event{Type: "text", Text: msg.Text})
		}

		// No tool calls => the agent considers this turn done.
		if len(msg.ToolCalls) == 0 {
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
				resultText = t.Func(tc.Arguments)
			} else {
				resultText = "ERROR: unknown tool " + tc.Name
			}
			a.emit(core.Event{Type: "tool_result", Tool: tc.Name, Result: resultText})
			s.messages = append(s.messages, core.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Text: resultText,
			})
		}

		// Loop detection: same call repeated LoopWindow times in a row.
		if len(recent) >= a.LoopWindow && allSame(recent[len(recent)-a.LoopWindow:]) {
			a.emit(core.Event{Type: "status", Status: "looping"})
			return RunResult{Status: "looping", Output: msg.Text,
				Turns: turn, InputTokens: inTok, OutputTokens: outTok}
		}
	}

	a.emit(core.Event{Type: "status", Status: "max_turns"})
	last := ""
	if n := len(s.messages); n > 0 {
		last = s.messages[n-1].Text
	}
	return RunResult{Status: "max_turns", Output: last,
		Turns: a.MaxTurns, InputTokens: inTok, OutputTokens: outTok}
}

// canonicalArgs serializes args deterministically (Go sorts map keys in JSON).
func canonicalArgs(args map[string]any) string {
	b, _ := json.Marshal(args)
	return string(b)
}

func allSame(xs []string) bool {
	for _, x := range xs {
		if x != xs[0] {
			return false
		}
	}
	return true
}
