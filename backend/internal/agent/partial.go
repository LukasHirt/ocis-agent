package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/textcap"
)

// partialEvery is the shortest time between two partial-answer writes of a
// job (Config.Now, so a fake clock drives it in tests).
const partialEvery = 500 * time.Millisecond

// partialSnap is what one SetJobPartial stores.
type partialSnap struct {
	state store.PartialState
	text  string
}

// partial collects the visible text of one streamed model call and stores
// it on the job (SetJobPartial) while the call runs, so any API process can
// show it. Only Delta.Text is kept; Delta.Thinking only sets the thinking
// state (the reasoning itself never reaches the runner). The text is
// untrusted and never the answer: the answer is the completed response.
//
// add runs on the runner goroutine (the provider calls onDelta there); a
// write runs in at most one goroutine at a time, so the stream never waits
// for the database, and wait joins it before the turn goes on. A delta that
// comes at least partialEvery after the previous write starts the next one,
// if the text or the state changed; final writes what is left at the end of
// a call whose turn goes on (tool calls). A lost lease or an ended job
// (store.ErrFenceLost, store.ErrConflict) cancels the model call through
// cancel and is returned by wait; other write errors are logged by category
// and ignored.
type partial struct {
	r      *Runner
	ctx    context.Context // the job's context: the writes outlive a cancelled call
	claim  store.Claim
	cancel context.CancelFunc

	// runner goroutine only
	buf      []byte
	thinking bool
	written  partialSnap // the last write started
	lastAt   time.Time
	inflight chan struct{} // closed when the running write is done; nil if none

	mu     sync.Mutex
	lost   error
	stored bool // a write succeeded: the job shows a partial
}

// add is the onDelta of the call.
func (p *partial) add(d llm.Delta) {
	if d.Thinking {
		p.thinking = true
	}
	if d.Text != "" && len(p.buf) <= store.MaxPartialBytes {
		// one byte over the cap is enough for snap to mark the cut (a rune
		// split there becomes U+FFFD, which is never shorter)
		p.buf = append(p.buf, d.Text[:min(len(d.Text), store.MaxPartialBytes+1-len(p.buf))]...)
	}
	if p.inflight != nil {
		select {
		case <-p.inflight:
			p.inflight = nil
		default:
			return // a write runs: the next delta tries again
		}
	}
	if p.lostErr() != nil {
		return
	}
	now := p.r.cfg.Now()
	if !p.lastAt.IsZero() && now.Sub(p.lastAt) < partialEvery {
		return
	}
	s := p.snap()
	if s == p.written {
		return
	}
	p.lastAt, p.written = now, s
	done := make(chan struct{})
	p.inflight = done
	p.r.partialWriters.Add(1)
	go func() {
		defer p.r.partialWriters.Add(-1)
		defer close(done)
		p.write(s)
	}()
}

// snap is the current state and the text capped at store.MaxPartialBytes.
func (p *partial) snap() partialSnap {
	switch {
	case len(p.buf) > 0:
		return partialSnap{store.PartialWriting, textcap.Cap(string(p.buf), store.MaxPartialBytes)}
	case p.thinking:
		return partialSnap{state: store.PartialThinking}
	}
	return partialSnap{}
}

// write stores s and records the outcome.
func (p *partial) write(s partialSnap) {
	err := p.r.setPartial(p.ctx, p.claim, s)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err == nil:
		p.stored = true
	case lost(err):
		p.lost = err
		p.cancel()
	}
}

func (p *partial) lostErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lost
}

// wait joins the running write and returns a lost lease.
func (p *partial) wait() error {
	if p.inflight != nil {
		<-p.inflight
		p.inflight = nil
	}
	return p.lostErr()
}

// final writes what changed since the last write, synchronously; it
// returns a lost lease.
func (p *partial) final() error {
	if err := p.wait(); err != nil {
		return err
	}
	if s := p.snap(); s != p.written {
		p.written = s
		p.write(s)
	}
	return p.lostErr()
}

// shown reports that the job may show a partial.
func (p *partial) shown() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stored
}

// setPartial stores s on the job. A lost lease or an ended job is returned;
// any other error is logged by category and dropped (nil), since the
// partial is only a preview.
func (r *Runner) setPartial(ctx context.Context, claim store.Claim, s partialSnap) error {
	err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
		return tx.SetJobPartial(claim.Job.UserID, claim.Job.ID, claim.Fence, s.state, s.text)
	})
	if err == nil || lost(err) {
		return err
	}
	if ctx.Err() == nil {
		r.log.Warn("agent runner: partial answer not saved", "job", claim.Job.ID, "outcome", "partial_error")
	}
	return nil
}

// complete makes the model call: streamed when the provider can stream,
// storing the partial answer meanwhile. The returned partial (nil without
// streaming) is already joined. A lost lease while streaming is returned
// wrapped and wins over the call's own outcome (lost reports it).
func (r *Runner) complete(ctx context.Context, claim store.Claim, req llm.CompletionRequest) (llm.CompletionResponse, *partial, error) {
	s, ok := r.d.Provider.(llm.Streamer)
	if !ok {
		resp, err := r.d.Provider.Complete(ctx, req)
		return resp, nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &partial{r: r, ctx: ctx, claim: claim, cancel: cancel}
	resp, err := s.CompleteStream(sctx, req, p.add)
	if lerr := p.wait(); lerr != nil {
		return llm.CompletionResponse{}, nil, fmt.Errorf("agent: save partial answer: %w", lerr)
	}
	return resp, p, err
}

// clearPartial clears the partial the turn's tool calls left, once they are
// processed (the text before a tool call is not the answer).
func (r *Runner) clearPartial(ctx context.Context, claim store.Claim) error {
	if err := r.setPartial(ctx, claim, partialSnap{}); err != nil {
		return fmt.Errorf("agent: clear partial answer: %w", err)
	}
	return nil
}
