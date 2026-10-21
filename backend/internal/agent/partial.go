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
// job, and streamCheckEvery between two re-reads of the kill switch and the
// policy while a call streams (both on Config.Now, so a fake clock drives
// them in tests).
const (
	partialEvery     = 500 * time.Millisecond
	streamCheckEvery = 5 * time.Second
)

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
//
// A delta (heartbeats included) at least streamCheckEvery after the start
// of the call or the previous check also starts a check, in at most one more
// goroutine: the kill switch, the pause and, for a proactive job, switched-off
// proactive work, read from the database like gateCheck. A hit cancels the
// call and is reported by stopped, so the job stops within seconds of the
// switch instead of when the call ends.
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
	checked  time.Time     // the start of the call or of the last check
	checking chan struct{} // closed when the running check is done; nil if none

	mu     sync.Mutex
	lost   error
	stored bool       // a write succeeded: the job shows a partial
	stop   stopReason // a check found the job must stop
}

// stopReason is why a check cancelled the call.
type stopReason int

const (
	stopNone stopReason = iota
	stopKillSwitch
	stopPaused
	stopProactiveOff
)

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
	now := p.r.cfg.Now()
	p.maybeCheck(now)
	p.maybeWrite(now)
}

// maybeCheck starts a check when the last one is old enough and none runs.
func (p *partial) maybeCheck(now time.Time) {
	if p.checking != nil {
		select {
		case <-p.checking:
			p.checking = nil
		default:
			return
		}
	}
	if p.lostErr() != nil || p.stopped() != stopNone || now.Sub(p.checked) < streamCheckEvery {
		return
	}
	p.checked = now
	done := make(chan struct{})
	p.checking = done
	p.r.partialWriters.Add(1)
	go func() {
		defer p.r.partialWriters.Add(-1)
		defer close(done)
		if why := p.r.streamCheck(p.ctx, p.claim); why != stopNone {
			p.mu.Lock()
			p.stop = why
			p.mu.Unlock()
			p.cancel()
		}
	}()
}

// maybeWrite starts a write when the throttle allows and the snapshot
// changed.
func (p *partial) maybeWrite(now time.Time) {
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
		if h := p.r.beforePartialWrite; h != nil {
			h()
		}
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

// wait joins the running write and check and returns a lost lease.
func (p *partial) wait() error {
	if p.inflight != nil {
		<-p.inflight
		p.inflight = nil
	}
	if p.checking != nil {
		<-p.checking
		p.checking = nil
	}
	return p.lostErr()
}

// stopped reports what a check found.
func (p *partial) stopped() stopReason {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stop
}

// streamCheck reads the kill switch and the policy like gateCheck. A read
// error is no reason to stop (the next check or turn reads again).
func (r *Runner) streamCheck(ctx context.Context, claim store.Claim) stopReason {
	on, err := KillSwitchOn(ctx, r.d.Store, r.cfg.KillSwitch)
	if err == nil && on {
		return stopKillSwitch
	}
	var pol store.Policy
	if err == nil {
		err = r.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			pol, err = tx.GetPolicy(claim.Job.UserID)
			return err
		})
	}
	switch {
	case err != nil:
		if ctx.Err() == nil {
			r.log.Warn("agent runner: stream check failed", "job", claim.Job.ID, "outcome", "check_error")
		}
		return stopNone
	case pol.Paused:
		return stopPaused
	case store.IsProactiveTrigger(claim.Job.TriggerKind) && !pol.ProactiveEnabled:
		return stopProactiveOff
	}
	return stopNone
}

// stopStream ends or requeues the job whose call a check cancelled, like
// gateCheck in a run, charging step (the call's usage, if any) in the same
// transaction.
func (r *Runner) stopStream(ctx context.Context, claim store.Claim, why stopReason, step *store.JobStep) error {
	switch why {
	case stopKillSwitch:
		return r.end(ctx, claim, ending{state: store.JobCancelled, reason: ReasonKillSwitch, step: step})
	case stopProactiveOff:
		return r.end(ctx, claim, ending{state: store.JobCancelled, reason: ReasonProactiveOff, step: step})
	}
	return r.requeueWith(ctx, claim, r.cfg.PausedDelay, step)
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
	p := &partial{r: r, ctx: ctx, claim: claim, cancel: cancel, checked: r.cfg.Now()}
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
