// Package llmtest is a scripted llm.Provider for tests.
package llmtest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

// Step is one scripted answer. Deltas are reported by a StreamProvider
// before it returns Resp or Err, each after waiting Gap.
type Step struct {
	Resp   llm.CompletionResponse
	Err    error
	Deltas []llm.Delta
	Gap    time.Duration
}

// Streamed adds the deltas a StreamProvider reports before it answers s.
func Streamed(s Step, deltas ...llm.Delta) Step {
	s.Deltas = append(append([]llm.Delta(nil), s.Deltas...), deltas...)
	return s
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

// Complete implements llm.Provider; it ignores the deltas of a step.
func (p *Provider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	s, err := p.next(ctx, req)
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	return s.Resp, s.Err
}

// next validates and records req and takes the next step.
func (p *Provider) next(ctx context.Context, req llm.CompletionRequest) (Step, error) {
	if err := ctx.Err(); err != nil {
		return Step{}, err
	}
	if err := req.Validate(); err != nil {
		return Step{}, err
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
		return Step{}, errors.New("llmtest: script exhausted")
	}
	s := p.steps[0]
	p.steps = p.steps[1:]
	return s, nil
}

// StreamProvider is a Provider that also implements llm.Streamer: it reports
// a step's deltas (waiting the step's Gap before each), stopping with the
// context error when ctx ends, then answers like Provider. For a model that
// reacts to the request or to the caller use llm.StreamFunc.
type StreamProvider struct {
	*Provider
}

// NewStreaming returns a streaming provider that plays steps in order.
func NewStreaming(steps ...Step) *StreamProvider { return &StreamProvider{New(steps...)} }

// CompleteStream implements llm.Streamer.
func (p *StreamProvider) CompleteStream(ctx context.Context, req llm.CompletionRequest, onDelta func(llm.Delta)) (llm.CompletionResponse, error) {
	s, err := p.next(ctx, req)
	if err != nil {
		return llm.CompletionResponse{}, err
	}
	for _, d := range s.Deltas {
		if s.Gap > 0 {
			t := time.NewTimer(s.Gap)
			select {
			case <-ctx.Done():
				t.Stop()
				return llm.CompletionResponse{}, ctx.Err()
			case <-t.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return llm.CompletionResponse{}, err
		}
		if onDelta != nil {
			onDelta(d)
		}
	}
	if err := ctx.Err(); err != nil {
		return llm.CompletionResponse{}, err
	}
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
