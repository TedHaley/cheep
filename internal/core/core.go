// Package core holds the provider-agnostic types shared across cheep.
//
// These are the lingua franca between the agent loop and the concrete
// providers. Each provider translates this normalized form to/from its native
// wire format, so the loop never has to know whether it is talking to Claude or
// Qwen.
package core

import "context"

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// Message is one conversation turn in normalized form.
type Message struct {
	Role       string // "user" | "assistant" | "tool"
	Text       string
	ToolCalls  []ToolCall // assistant only
	ToolCallID string     // tool role only
	Name       string     // tool name, for the tool role
}

// Turn is one assistant response plus its usage accounting.
type Turn struct {
	Message      Message
	InputTokens  int
	OutputTokens int
}

// Tool is a callable exposed to an agent. Func receives the run's context so
// long-running tools (bash, delegate) abort when the run is cancelled.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema for the arguments object
	Func        func(context.Context, map[string]any) string
}

// Provider is one model backend (Anthropic, OpenAI-compatible, ...).
type Provider interface {
	Complete(ctx context.Context, model, system string, messages []Message, tools []Tool) (Turn, error)
}

// Event is emitted during an agent run for live display. The json tags define
// the wire shape of `cheep run --json` output — treat them as a public API.
type Event struct {
	Agent  string         `json:"agent"`
	Type   string         `json:"type"` // "text" | "tool_call" | "tool_result" | "status" | "error" | "progress"
	Text   string         `json:"text,omitempty"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Result string         `json:"result,omitempty"`
	Status string         `json:"status,omitempty"`
	Turn   int            `json:"turn,omitempty"`          // progress: current turn
	Tokens int            `json:"tokens,omitempty"`        // progress: cumulative input tokens
	Model  string         `json:"model,omitempty"`         // usage: the model that produced this turn
	InTok  int            `json:"input_tokens,omitempty"`  // usage: input tokens this turn
	OutTok int            `json:"output_tokens,omitempty"` // usage: output tokens this turn
}

// EventFunc receives run events.
type EventFunc func(Event)
