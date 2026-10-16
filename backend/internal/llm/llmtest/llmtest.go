// Package llmtest is a scripted llm.Provider for tests.
package llmtest

import (
	"context"
	"errors"
	"sync"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

// Step is one scripted answer.
type Step struct {
	Resp llm.CompletionResponse
	Err  error
}

// Say answers with plain text.
func Say(text string, in, out int) Step {
	return Step{Resp: llm.CompletionResponse{
		Message: llm.Message{Role: llm.RoleAssistant, Content: text},
		Usage:   llm.Usage{In: in, Out: out},
		Stop:    llm.StopEnd,
	}}
}

// Calls answers with tool calls.
func Calls(in, out int, calls ...llm.ToolCall) Step {
	return Step{Resp: llm.CompletionResponse{
		Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: calls},
		Usage:   llm.Usage{In: in, Out: out},
		Stop:    llm.StopToolCalls,
	}}
}

// Fail answers with an error.
func Fail(err error) Step { return Step{Err: err} }

// Provider plays its steps in order. It validates every request, like a real
// provider, and records a copy of each.
type Provider struct {
	mu    sync.Mutex
	steps []Step
	seen  []llm.CompletionRequest
}

// New returns a provider that plays steps in order.
func New(steps ...Step) *Provider { return &Provider{steps: steps} }

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.CompletionResponse{}, err
	}
	if err := req.Validate(); err != nil {
		return llm.CompletionResponse{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	msgs := make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		m.ToolCalls = append([]llm.ToolCall(nil), m.ToolCalls...)
		msgs[i] = m
	}
	req.Messages = msgs
	tools := make([]llm.ToolSpec, len(req.Tools))
	for i, t := range req.Tools {
		t.Schema = append([]byte(nil), t.Schema...)
		tools[i] = t
	}
	req.Tools = tools
	p.seen = append(p.seen, req)
	if len(p.steps) == 0 {
		return llm.CompletionResponse{}, errors.New("llmtest: script exhausted")
	}
	s := p.steps[0]
	p.steps = p.steps[1:]
	return s.Resp, s.Err
}

// Requests returns the requests received so far.
func (p *Provider) Requests() []llm.CompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.CompletionRequest(nil), p.seen...)
}

// Remaining is the number of unplayed steps.
func (p *Provider) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.steps)
}
