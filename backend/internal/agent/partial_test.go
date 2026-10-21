package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/textcap"
)

// partialWrite is one recorded SetJobPartial call.
type partialWrite struct {
	state store.PartialState
	text  string
	at    time.Time
	err   error
}

func (t faultTx) SetJobPartial(userID, jobID string, fence int64, state store.PartialState, text string) error {
	err := t.s.hit("SetJobPartial")
	if err == nil {
		err = t.Tx.SetJobPartial(userID, jobID, fence, state, text)
	}
	t.s.mu.Lock()
	t.s.partials = append(t.s.partials, partialWrite{state: state, text: text, at: t.s.now(), err: err})
	t.s.mu.Unlock()
	return err
}

// writes returns the recorded SetJobPartial calls.
func (f *fixture) writes() []partialWrite {
	f.faults.mu.Lock()
	defer f.faults.mu.Unlock()
	return append([]partialWrite(nil), f.faults.partials...)
}

// pacedDelta is a delta the fake clock reaches after moving by after.
// before, if set, runs right before the delta.
type pacedDelta struct {
	after  time.Duration
	d      llm.Delta
	before func()
}

func text(after time.Duration, s string) pacedDelta {
	return pacedDelta{after: after, d: llm.Delta{Text: s}}
}

func thinking(after time.Duration) pacedDelta {
	return pacedDelta{after: after, d: llm.Delta{Thinking: true}}
}

// then runs fn right before d.
func (d pacedDelta) then(fn func()) pacedDelta { d.before = fn; return d }

// pacedStep is one scripted streamed call: the deltas, then (after every
// write the deltas started has finished) during, then the answer.
type pacedStep struct {
	deltas []pacedDelta
	during func(ctx context.Context)
	resp   llm.CompletionResponse
	err    error
}

// pacedStreamer is a scripted llm.Streamer on the fixture clock. Before each
// delta (and before during) it waits until no partial write of the runner
// runs, so what the runner writes depends only on the clock.
type pacedStreamer struct {
	f     *fixture
	mu    sync.Mutex
	r     *Runner
	steps []pacedStep
	sent  int // deltas handed to onDelta
}

func (p *pacedStreamer) Complete(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
	p.f.t.Error("the runner must stream when the provider can")
	return llm.CompletionResponse{}, llm.ErrUnavailable
}

func (p *pacedStreamer) settle() {
	deadline := time.Now().Add(5 * time.Second)
	for p.r.partialWriters.Load() != 0 {
		if time.Now().After(deadline) {
			p.f.t.Fatal("a partial write does not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *pacedStreamer) CompleteStream(ctx context.Context, req llm.CompletionRequest, onDelta func(llm.Delta)) (llm.CompletionResponse, error) {
	if err := req.Validate(); err != nil {
		return llm.CompletionResponse{}, err
	}
	p.mu.Lock()
	if len(p.steps) == 0 {
		p.mu.Unlock()
		return llm.CompletionResponse{}, errors.New("script exhausted")
	}
	s := p.steps[0]
	p.steps = p.steps[1:]
	p.mu.Unlock()
	for _, d := range s.deltas {
		p.settle()
		if err := ctx.Err(); err != nil {
			return llm.CompletionResponse{}, err
		}
		if d.before != nil {
			d.before()
		}
		p.f.clk.Advance(d.after)
		onDelta(d.d)
		p.sent++
	}
	p.settle()
	if s.during != nil {
		s.during(ctx)
	}
	if err := ctx.Err(); err != nil {
		return llm.CompletionResponse{}, err
	}
	return s.resp, s.err
}

func say(text string, in, out int) llm.CompletionResponse { return llmtest.Say(text, in, out).Resp }

func calls(in, out int, c ...llm.ToolCall) llm.CompletionResponse {
	return llmtest.Calls(in, out, c...).Resp
}

// streaming sets a paced provider and returns it with its runner.
func (f *fixture) streaming(steps ...pacedStep) (*pacedStreamer, *Runner) {
	f.t.Helper()
	p := &pacedStreamer{f: f, steps: steps}
	f.provider = p
	r := f.runner()
	p.r = r
	return p, r
}

// partialNow returns the job's stored partial.
func (f *fixture) partialNow() store.JobPartial { return f.job().JobPartial }

func (f *fixture) noWriterLeft(r *Runner) {
	f.t.Helper()
	if n := r.partialWriters.Load(); n != 0 {
		f.t.Errorf("%d partial writes still run", n)
	}
}

type seenWrite struct {
	state store.PartialState
	text  string
	at    time.Duration // since start
}

func (f *fixture) writesSince(start time.Time) []seenWrite {
	var out []seenWrite
	for _, x := range f.writes() {
		if x.err != nil {
			f.t.Errorf("write error: %v", x.err)
		}
		out = append(out, seenWrite{x.state, x.text, x.at.Sub(start)})
	}
	return out
}

func wantWrites(t *testing.T, got, want []seenWrite) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("writes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

const ms100 = 100 * time.Millisecond

// The partial answer is written at most every 500 ms and only when it
// changed, flushed at the end of a tool-call turn, cleared once the calls
// are processed and by the release of the final answer, which is the
// completed response and never the streamed text.
func TestPartialAnswerIsThrottledFlushedAndCleared(t *testing.T) {
	f := newFixture(t)
	conv := f.newConversation()
	f.postChat(conv, "what is in my drive?")
	var mid1, mid2 store.JobPartial
	_, r := f.streaming(
		pacedStep{
			deltas: []pacedDelta{thinking(0), thinking(ms100), text(ms100, "Let me"), text(ms100, " look"), text(2*ms100, " at"), text(ms100, "!")},
			during: func(context.Context) { mid1 = f.partialNow() },
			resp:   calls(5, 2, list("x1", "/")),
		},
		pacedStep{
			deltas: []pacedDelta{text(0, "Do"), text(ms100, "ne")},
			during: func(context.Context) { mid2 = f.partialNow() },
			resp:   say("Done.", 7, 3),
		},
	)
	start := f.clk.Now()
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.noWriterLeft(r)
	wantWrites(t, f.writesSince(start), []seenWrite{
		{store.PartialThinking, "", 0},                       // the first delta writes at once
		{store.PartialWriting, "Let me look at", 5 * ms100},  // the first delta 500 ms later
		{store.PartialWriting, "Let me look at!", 6 * ms100}, // the final flush before the calls run
		{store.PartialNone, "", 6 * ms100},                   // cleared once the calls are processed
		{store.PartialWriting, "Do", 6 * ms100},              // a new call writes at once
	})
	if mid1.PartialState != store.PartialWriting || mid1.Partial != "Let me look at" || mid1.PartialAt.IsZero() {
		t.Errorf("during the first call: %+v", mid1)
	}
	if mid2.PartialState != store.PartialWriting || mid2.Partial != "Do" {
		t.Errorf("during the second call: %+v", mid2)
	}
	j := f.job()
	if j.State != store.JobSucceeded || j.JobPartial != (store.JobPartial{}) {
		t.Fatalf("job %s with %+v", j.State, j.JobPartial)
	}
	// the answer is the completed response, not the streamed text
	if m := f.jobMessages(conv, j.ID); len(m) != 1 || m[0].Content != "Done." {
		t.Errorf("messages = %+v", m)
	}
	if cp := f.checkpoint(); cp.Answer != "Done." {
		t.Errorf("checkpoint answer %q", cp.Answer)
	}
	// the usage of both streamed calls is recorded as before
	var in, out int64
	for _, s := range f.steps() {
		in, out = in+s.TokensIn, out+s.TokensOut
	}
	if in != 12 || out != 5 {
		t.Errorf("tokens = %d/%d", in, out)
	}
}

// Nothing is written while neither the text nor the state changed, and a
// thinking delta after text keeps the writing state.
func TestPartialAnswerSkipsUnchangedWrites(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	_, r := f.streaming(pacedStep{
		deltas: []pacedDelta{text(0, "a"), text(6*ms100, ""), thinking(6 * ms100), text(6*ms100, "b")},
		resp:   say("ab", 1, 1),
	})
	start := f.clk.Now()
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
	f.noWriterLeft(r)
	wantWrites(t, f.writesSince(start), []seenWrite{
		{store.PartialWriting, "a", 0},
		{store.PartialWriting, "ab", 18 * ms100},
	})
	if j := f.job(); j.State != store.JobSucceeded || j.JobPartial != (store.JobPartial{}) {
		t.Errorf("job %s %+v", j.State, j.JobPartial)
	}
}

// A call that fails after streaming half an answer leaves nothing behind:
// the partial is cleared by the requeue (retryable) or the failure, and the
// half answer is never the answer.
func TestPartialAnswerOfAFailedCallIsDiscarded(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		state store.JobState
	}{
		{"retryable", llm.ErrUnavailable, store.JobQueued},
		{"rejected", llm.ErrRejected, store.JobFailed},
		{"malformed", llm.ErrMalformed, store.JobFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			conv := f.newConversation()
			id := f.postChat(conv, "g")
			var mid store.JobPartial
			_, r := f.streaming(pacedStep{
				deltas: []pacedDelta{text(0, "half an ans")},
				during: func(context.Context) { mid = f.partialNow() },
				err:    c.err,
			})
			if err := r.Run(bg, f.claim()); err != nil {
				t.Fatal(err)
			}
			f.noWriterLeft(r)
			if mid.Partial != "half an ans" {
				t.Errorf("during the call: %+v", mid)
			}
			j := f.job()
			if j.State != c.state || j.JobPartial != (store.JobPartial{}) {
				t.Fatalf("job %s with %+v", j.State, j.JobPartial)
			}
			if c.state == store.JobQueued {
				assertRequeued(t, f, DefaultConfig().RequeueDelay)
			}
			for _, m := range f.jobMessages(conv, id) {
				if strings.Contains(m.Content, "half") {
					t.Errorf("the half answer became a message: %+v", m)
				}
			}
			if strings.Contains(string(j.Checkpoint), "half") {
				t.Errorf("the half answer is in the checkpoint: %s", j.Checkpoint)
			}
		})
	}
}

// A lost lease stops the call: the write that finds it cancels the stream,
// Run reports the lost fence, nothing of the turn is recorded and the new
// holder's job shows nothing.
func TestPartialAnswerStopsOnALostLease(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	var c2 store.Claim
	p, r := f.streaming(pacedStep{
		deltas: []pacedDelta{
			text(0, "a"),
			text(6*ms100, "b").then(func() { c2 = f.takeover() }),
			text(6*ms100, "c"), text(6*ms100, "d"),
		},
		resp: say("abcd", 1, 1),
	})
	err := r.Run(bg, f.claim())
	if !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("Run = %v, want ErrFenceLost", err)
	}
	f.noWriterLeft(r)
	if p.sent != 2 {
		t.Errorf("%d deltas sent, want the stream to stop after the failed write", p.sent)
	}
	j := f.job()
	if j.State != store.JobRunning || j.JobPartial != (store.JobPartial{}) || c2.Job.ID != j.ID {
		t.Errorf("job %s with %+v", j.State, j.JobPartial)
	}
	if len(f.steps()) != 0 {
		t.Errorf("steps = %+v", f.steps())
	}
}

// A job the user cancels while its answer streams: the next write finds it
// ended, the call stops and the cancel has cleared the partial.
func TestPartialAnswerStopsWhenTheJobIsCancelled(t *testing.T) {
	f := newFixture(t)
	conv := f.newConversation()
	id := f.postChat(conv, "g")
	p, r := f.streaming(pacedStep{
		deltas: []pacedDelta{
			text(0, "a"),
			text(6*ms100, "b").then(func() {
				if err := f.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, id) }); err != nil {
					t.Error(err)
				}
			}),
			text(6*ms100, "c"),
		},
		resp: say("abc", 1, 1),
	})
	err := r.Run(bg, f.claim())
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Run = %v, want ErrConflict", err)
	}
	f.noWriterLeft(r)
	if p.sent != 2 {
		t.Errorf("%d deltas sent", p.sent)
	}
	if j := f.job(); j.State != store.JobCancelled || j.JobPartial != (store.JobPartial{}) {
		t.Errorf("job %s with %+v", j.State, j.JobPartial)
	}
	if ms := f.jobMessages(conv, id); len(ms) != 0 {
		t.Errorf("messages = %+v", ms)
	}
}

// The kill switch or a pause set while the answer streams does not cut the
// call off, but stops the job before its tool calls run, and the release
// clears the partial.
func TestPartialAnswerIsClearedWhenTheKillSwitchOrAPauseStopsTheJob(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   func(f *fixture) error
		state store.JobState
	}{
		{"kill switch", func(f *fixture) error { return SetKillSwitch(bg, f.st, true) }, store.JobCancelled},
		{"pause", func(f *fixture) error {
			return f.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(userID, store.PauseUser); return err })
		}, store.JobQueued},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.enqueue("g")
			var mid store.JobPartial
			_, r := f.streaming(pacedStep{
				deltas: []pacedDelta{
					text(0, "I will list"),
					text(ms100, " it").then(func() {
						if err := c.set(f); err != nil {
							t.Error(err)
						}
					}),
				},
				during: func(context.Context) { mid = f.partialNow() },
				resp:   calls(1, 1, list("x1", "/")),
			})
			if err := r.Run(bg, f.claim()); err != nil {
				t.Fatal(err)
			}
			f.noWriterLeft(r)
			if mid.Partial != "I will list" {
				t.Errorf("during the call: %+v", mid)
			}
			if j := f.job(); j.State != c.state || j.JobPartial != (store.JobPartial{}) {
				t.Errorf("job %s with %+v", j.State, j.JobPartial)
			}
			if n := f.executes("files.list"); n != 0 {
				t.Errorf("the call ran %d times", n)
			}
		})
	}
}

// A cancelled run context (shutdown) ends the call as a context error; the
// worker's requeue then clears the partial.
func TestPartialAnswerOnShutdown(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	_, r := f.streaming(pacedStep{
		deltas: []pacedDelta{text(0, "a"), text(ms100, "b").then(cancel), text(ms100, "c")},
		resp:   say("abc", 1, 1),
	})
	c := f.claim()
	if err := r.Run(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	f.noWriterLeft(r)
	if p := f.partialNow(); p.Partial != "a" {
		t.Errorf("partial = %+v", p)
	}
	if err := f.st.InTx(bg, func(tx store.Tx) error { _, err := tx.RequeueJob(c, f.clk.Now(), nil); return err }); err != nil {
		t.Fatal(err)
	}
	if j := f.job(); j.State != store.JobQueued || j.JobPartial != (store.JobPartial{}) {
		t.Errorf("job %s with %+v", j.State, j.JobPartial)
	}
}

// A write that fails for another reason is dropped: the stream goes on, the
// next write is tried and the job ends normally.
func TestAFailedPartialWriteIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	f.faults.failAt("SetJobPartial", 1)
	_, r := f.streaming(pacedStep{
		deltas: []pacedDelta{text(0, "a"), text(6*ms100, "b")},
		resp:   say("ab", 1, 1),
	})
	start := f.clk.Now()
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
	f.noWriterLeft(r)
	got := f.writes()
	if len(got) != 2 || !errors.Is(got[0].err, errInjected) || got[1].err != nil || got[1].text != "ab" || got[1].at.Sub(start) != 6*ms100 {
		t.Errorf("writes = %+v", got)
	}
	if j := f.job(); j.State != store.JobSucceeded || j.JobPartial != (store.JobPartial{}) {
		t.Errorf("job %s with %+v", j.State, j.JobPartial)
	}
}

// The stored partial is capped like an answer and ends in the marker; the
// answer itself is the response, uncut by the partial cap.
func TestPartialAnswerIsCapped(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	chunk := strings.Repeat("ä", 10000) // 20000 bytes
	var deltas []pacedDelta
	for range 5 {
		deltas = append(deltas, text(6*ms100, chunk))
	}
	var mid store.JobPartial
	_, r := f.streaming(pacedStep{
		deltas: deltas,
		during: func(context.Context) { mid = f.partialNow() },
		resp:   calls(1, 1, list("x1", "/")),
	}, pacedStep{resp: say("short", 1, 1)})
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
	f.noWriterLeft(r)
	if len(mid.Partial) > store.MaxPartialBytes || !strings.HasSuffix(mid.Partial, textcap.Marker) {
		t.Errorf("partial of %d bytes, suffix %q", len(mid.Partial), mid.Partial[max(0, len(mid.Partial)-20):])
	}
	for _, w := range f.writes() {
		if w.err != nil || len(w.text) > store.MaxPartialBytes {
			t.Errorf("write of %d bytes: %v", len(w.text), w.err)
		}
	}
}

// An estimated usage (a stream without usage) is recorded like a reported
// one and counts against the daily budget.
func TestStreamedEstimatedUsageIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	resp := say("hi", 40, 9)
	resp.UsageEstimated = true
	_, r := f.streaming(pacedStep{deltas: []pacedDelta{text(0, "hi")}, resp: resp})
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
	s := f.steps()
	if len(s) != 1 || s[0].TokensIn != 40 || s[0].TokensOut != 9 {
		t.Fatalf("steps = %+v", s)
	}
	var in, out int64
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		in, out, err = tx.SumJobTokens(userID, startOfDay(f.clk.Now()))
		return err
	}); err != nil || in != 40 || out != 9 {
		t.Errorf("SumJobTokens = %d/%d, %v", in, out, err)
	}
}

// A provider without streaming writes no partial.
func TestNoPartialWithoutStreaming(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	f.provider = llmtest.New(llmtest.Calls(1, 1, list("x1", "/")), llmtest.Say("ok", 1, 1))
	f.runOnce()
	if w := f.writes(); len(w) != 0 {
		t.Errorf("writes = %+v", w)
	}
	if j := f.job(); j.State != store.JobSucceeded {
		t.Errorf("job %s", j.State)
	}
}

// The scripted streaming provider of llmtest drives the runner too.
func TestTheRunnerStreamsWithTheScriptedStreamingProvider(t *testing.T) {
	f := newFixture(t)
	f.enqueue("g")
	f.provider = llmtest.NewStreaming(llmtest.Streamed(llmtest.Say("Hello", 3, 2), llm.Delta{Thinking: true}, llm.Delta{Text: "Hel"}))
	r := f.runner()
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
	f.noWriterLeft(r)
	if w := f.writes(); len(w) != 1 || w[0].state != store.PartialThinking {
		t.Errorf("writes = %+v", w)
	}
	if j := f.job(); j.State != store.JobSucceeded || j.JobPartial != (store.JobPartial{}) {
		t.Errorf("job %s with %+v", j.State, j.JobPartial)
	}
}

// A call that fails after its stream started returns the estimated usage
// with the error: it is charged (an llm step "failed" in the transaction
// that requeues or fails the job, or right away when the run context ended),
// so failing calls cannot spend the budget unseen.
func TestTheUsageOfAFailedStreamIsCharged(t *testing.T) {
	spent := llm.CompletionResponse{Usage: llm.Usage{In: 30, Out: 4}, UsageEstimated: true}
	for _, c := range []struct {
		name  string
		err   error
		state store.JobState
	}{
		{"retryable", llm.ErrUnavailable, store.JobQueued},
		{"rejected", llm.ErrRejected, store.JobFailed},
		{"run context ended", context.Canceled, store.JobRunning},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.enqueue("g")
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			f.provider = llm.StreamFunc(func(ctx2 context.Context, req llm.CompletionRequest, onDelta func(llm.Delta)) (llm.CompletionResponse, error) {
				onDelta(llm.Delta{Text: "half"})
				if errors.Is(c.err, context.Canceled) {
					cancel()
					return spent, ctx2.Err()
				}
				return spent, c.err
			})
			r := f.runner()
			err := r.Run(ctx, f.claim())
			if errors.Is(c.err, context.Canceled) != (err != nil) {
				t.Fatalf("Run = %v", err)
			}
			f.noWriterLeft(r)
			j := f.job()
			if j.State != c.state {
				t.Fatalf("job %s", j.State)
			}
			s := f.steps()
			if len(s) != 1 || s[0].Kind != "llm" || s[0].Status != "failed" || s[0].TokensIn != 30 || s[0].TokensOut != 4 {
				t.Fatalf("steps = %+v", s)
			}
			var in, out int64
			if err := f.st.View(bg, func(tx store.Tx) error {
				var err error
				in, out, err = tx.SumJobTokens(userID, startOfDay(f.clk.Now()))
				return err
			}); err != nil || in != 30 || out != 4 {
				t.Errorf("SumJobTokens = %d/%d, %v", in, out, err)
			}
		})
	}
	t.Run("an error without usage records no step", func(t *testing.T) {
		f := newFixture(t)
		f.enqueue("g")
		f.provider = llmtest.NewStreaming(llmtest.Streamed(llmtest.Fail(llm.ErrUnavailable), llm.Delta{Text: "x"}))
		f.runOnce()
		if s := f.steps(); len(s) != 0 {
			t.Errorf("steps = %+v", s)
		}
	})
}
