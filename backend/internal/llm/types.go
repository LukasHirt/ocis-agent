// Package llm defines the model provider interface and an OpenAI-compatible
// client. Output of a provider is untrusted model text.
package llm

import (
	"context"
	"encoding/json"
)

// Role is the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one tool invocation requested by the model. Arguments is the
// raw string the model produced; it may not be valid JSON, and the approval
// gate parses and canonicalises it.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one turn of the transcript. ToolCalls is set on assistant
// messages, ToolCallID on tool results.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolSpec describes a tool to the model. Schema is a JSON Schema object;
// empty means "no parameters".
type ToolSpec struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// CompletionRequest is one model call. Model, when set, overrides the model
// the client is configured with; the caller must have taken it from the
// operator's Catalog.
type CompletionRequest struct {
	Model     string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int // 0 lets the provider decide
}

// StopReason says why the model stopped.
type StopReason string

const (
	StopEnd       StopReason = "stop"
	StopToolCalls StopReason = "tool_calls"
	// StopLength means the output was cut off; tool-call arguments in such a
	// response may be truncated and must not be executed.
	StopLength StopReason = "length"
)

// Usage is the token accounting of one call.
type Usage struct {
	In  int
	Out int
}

// CompletionResponse is the model's answer.
type CompletionResponse struct {
	Message Message
	Usage   Usage
	Stop    StopReason
}

// Provider is a model backend.
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// ProviderFunc adapts a function to Provider (handy for scripted models that
// react to the request).
type ProviderFunc func(context.Context, CompletionRequest) (CompletionResponse, error)

// Complete calls f.
func (f ProviderFunc) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	return f(ctx, req)
}
