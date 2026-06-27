// Package core holds the provider-agnostic types shared across cheep.
//
// These are the lingua franca between the agent loop and the concrete
// providers. Each provider translates this normalized form to/from its native
// wire format, so the loop never has to know whether it is talking to Claude or
// Qwen.
package core

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

// Tool is a callable exposed to an agent.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema for the arguments object
	Func        func(map[string]any) string
}

// Provider is one model backend (Anthropic, OpenAI-compatible, ...).
type Provider interface {
	Complete(model, system string, messages []Message, tools []Tool) (Turn, error)
}

// Event is emitted during an agent run for live display.
type Event struct {
	Agent  string
	Type   string // "text" | "tool_call" | "tool_result" | "status" | "error"
	Text   string
	Tool   string
	Args   map[string]any
	Result string
	Status string
}

// EventFunc receives run events.
type EventFunc func(Event)
