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
	// UsageEstimated means the provider streamed no usage (or not all of it)
	// and the missing part of Usage is a rough estimate (about three bytes a
	// token, never zero).
	UsageEstimated bool
}

// Provider is a model backend.
type Provider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// Delta is a piece of a streamed answer: visible text the model wrote, or
// Thinking when the model started its hidden reasoning (reported once; the
// reasoning text itself is never passed on). Text is untrusted model output
// and may belong to a call that fails later: it is never the final answer.
type Delta struct {
	Text     string
	Thinking bool
}

// Streamer is a Provider that can report the answer while it is written.
// CompleteStream calls onDelta (which may be nil) on the goroutine that reads
// the response, and returns the same response as Complete. onDelta must not
// block: the idle timeout keeps running while it does, so a slow callback
// (a database write waiting for a lock) ends a healthy call as an outage.
// Copy the delta and return. onDelta may never be called, for example when
// the endpoint does not stream. On an error the response may still carry
// the estimated Usage of a stream that had started (UsageEstimated set,
// nothing else), so that a caller can charge the tokens spent.
type Streamer interface {
	Provider
	CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(Delta)) (CompletionResponse, error)
}

// ProviderFunc adapts a function to Provider (handy for scripted models that
// react to the request).
type ProviderFunc func(context.Context, CompletionRequest) (CompletionResponse, error)

// Complete calls f.
func (f ProviderFunc) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	return f(ctx, req)
}

// StreamFunc adapts a function to Streamer; Complete calls it with a nil
// onDelta.
type StreamFunc func(ctx context.Context, req CompletionRequest, onDelta func(Delta)) (CompletionResponse, error)

// Complete calls f without a delta callback.
func (f StreamFunc) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	return f(ctx, req, nil)
}

// CompleteStream calls f.
func (f StreamFunc) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(Delta)) (CompletionResponse, error) {
	return f(ctx, req, onDelta)
}
