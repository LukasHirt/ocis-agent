package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// wait is the failure guard for every wait in these tests: a correct worker
// gets there in milliseconds.
const wait = 10 * time.Second

const workerTTL = 60 * time.Millisecond

// spyStore records the lease operations that reach the database: claims,
// renewals and releases (per fence), and signals each renewal and each empty
// claim.
type spyStore struct {
	store.Store
	mu       sync.Mutex
	claimed  []store.Claim
	renewed  []store.Claim
	releases map[int64]int // ReleaseJob and RequeueJob calls by fence
	empty    int
	renewCh  chan store.Claim
	emptyCh  chan struct{}
	// renewFail fails that many of the next RenewLease calls with
	// errInjected, a transient error; failCh signals each.
	renewFail atomic.Int32
	failCh    chan struct{}
}

func newSpy(st store.Store) *spyStore {
	return &spyStore{Store: st, releases: map[int64]int{}, renewCh: make(chan store.Claim, 1024), emptyCh: make(chan struct{}, 1024), failCh: make(chan struct{}, 1024)}
}

func (s *spyStore) InTx(ctx context.Context, fn func(store.Tx) error) error {
	return s.Store.InTx(ctx, func(tx store.Tx) error { return fn(spyTx{Tx: tx, s: s}) })
}

func (s *spyStore) claims() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.claimed)
}

func (s *spyStore) firstClaim() store.Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed[0]
}

func (s *spyStore) renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.renewed)
}

func (s *spyStore) releasesOf(fence int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releases[fence]
}

type spyTx struct {
	store.Tx
	s *spyStore
}

func (t spyTx) ClaimNextJob(worker string, ttl time.Duration) (store.Claim, bool, error) {
	c, ok, err := t.Tx.ClaimNextJob(worker, ttl)
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if err == nil && ok {
		t.s.claimed = append(t.s.claimed, c)
	}
	if err == nil && !ok {
		t.s.empty++
		select {
		case t.s.emptyCh <- struct{}{}:
		default:
		}
	}
	return c, ok, err
}

func (t spyTx) RenewLease(c store.Claim, ttl time.Duration) (store.Claim, error) {
	if t.s.renewFail.Add(-1) >= 0 {
		t.s.failCh <- struct{}{}
		return store.Claim{}, errInjected
	}
	t.s.renewFail.Store(0)
	out, err := t.Tx.RenewLease(c, ttl)
	if err == nil {
		t.s.mu.Lock()
		t.s.renewed = append(t.s.renewed, out)
		t.s.mu.Unlock()
		select {
		case t.s.renewCh <- out:
		default:
		}
	}
	return out, err
}

func (t spyTx) ReleaseJob(c store.Claim, next store.JobState, cp json.RawMessage, msg string) (bool, error) {
	t.s.mu.Lock()
	t.s.releases[c.Fence]++
	t.s.mu.Unlock()
	return t.Tx.ReleaseJob(c, next, cp, msg)
}

func (t spyTx) RequeueJob(c store.Claim, at time.Time, cp json.RawMessage) (bool, error) {
	t.s.mu.Lock()
	t.s.releases[c.Fence]++
	t.s.mu.Unlock()
	return t.Tx.RequeueJob(c, at, cp)
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// ending is what the worker reported for one processed claim.
type ended struct {
	jobID   string
	outcome string
	live    int32 // renewal goroutines when the claim counted as processed
}

type workerFixture struct {
	*fixture
	spy   *spyStore
	ended chan ended
	log   *syncBuffer
}

// newWorkerFixture is the runner fixture with the store behind a spy and a
// queue with a short lease and poll interval. The store's clock is the
// fixture clock, so leases expire only when a test advances it; renewals
// tick in real time.
func newWorkerFixture(t *testing.T) *workerFixture {
	f := newFixture(t)
	spy := newSpy(f.st)
	f.st = spy
	f.q = queue.New(spy, queue.WithLeaseTTL(workerTTL), queue.WithPollInterval(10*time.Millisecond))
	f.cfg.RequeueDelay = 7 * time.Second
	return &workerFixture{fixture: f, spy: spy, ended: make(chan ended, 64), log: &syncBuffer{}}
}

func (f *workerFixture) worker(id string) *Worker {
	f.t.Helper()
	w, err := NewWorker(f.q, f.st, f.runner(), WorkerConfig{
		ID:  id,
		Log: slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	w.killPoll = 5 * time.Millisecond
	w.onDone = func(c store.Claim, outcome string) { f.ended <- ended{c.Job.ID, outcome, w.live.Load()} }
	return w
}

// start runs w until the returned stop is called (or the test ends) and
// checks that Run returns and leaves no goroutine behind.
func (f *workerFixture) start(ws ...*Worker) (stop func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunPool(ctx, len(ws), func(i int) *Worker { return ws[i] })
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(wait):
				f.t.Error("the worker did not stop")
				return
			}
			for _, w := range ws {
				if n := w.live.Load(); n != 0 {
					f.t.Errorf("%d goroutines of the worker still run", n)
				}
			}
		})
	}
	f.t.Cleanup(stop)
	return stop
}

// next waits for the worker to finish processing a claim.
func (f *workerFixture) next() ended {
	f.t.Helper()
	select {
	case e := <-f.ended:
		return e
	case <-time.After(wait):
		f.t.Fatal("no claim was processed")
	}
	return ended{}
}

func (f *workerFixture) expect(jobID, outcome string) {
	f.t.Helper()
	e := f.next()
	if e.jobID != jobID || e.outcome != outcome {
		f.t.Fatalf("processed %+v, want %s %s", e, jobID, outcome)
	}
	// the renewal stops before the claim counts as processed
	if e.live != 0 {
		f.t.Errorf("%d renewal goroutines still run after %s", e.live, outcome)
	}
}

// blocking is a provider that signals each call and blocks until its ctx
// ends, then returns the ctx error (wrapped, with text) and records the cause.
type blocking struct {
	entered chan struct{}
	cause   chan error
}

func newBlocking() *blocking {
	return &blocking{entered: make(chan struct{}, 16), cause: make(chan error, 16)}
}

func (b *blocking) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	b.entered <- struct{}{}
	<-ctx.Done()
	b.cause <- context.Cause(ctx)
	return llm.CompletionResponse{}, fmt.Errorf("SECRET-provider-text: %w", ctx.Err())
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(wait):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestNewWorkerChecksItsConfig(t *testing.T) {
	f := newWorkerFixture(t)
	f.provider = llmtest.New()
	r := f.runner()
	ok := WorkerConfig{ID: "w1"}
	if _, err := NewWorker(f.q, f.st, r, ok); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]func() (*queue.Queue, store.Store, *Runner, WorkerConfig){
		"no queue":  func() (*queue.Queue, store.Store, *Runner, WorkerConfig) { return nil, f.st, r, ok },
		"no store":  func() (*queue.Queue, store.Store, *Runner, WorkerConfig) { return f.q, nil, r, ok },
		"no runner": func() (*queue.Queue, store.Store, *Runner, WorkerConfig) { return f.q, f.st, nil, ok },
		"no id": func() (*queue.Queue, store.Store, *Runner, WorkerConfig) {
			return f.q, f.st, r, WorkerConfig{}
		},
	} {
		if _, err := NewWorker(c()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestAWorkerRunsAClaimedJobToTheEnd(t *testing.T) {
	f := newWorkerFixture(t)
	f.provider = llmtest.New(llmtest.Say("done", 1, 1))
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	f.expect(id, "released")
	if j := f.job(); j.State != store.JobSucceeded {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if cp := f.checkpoint(); cp.Answer != "done" {
		t.Errorf("answer = %q", cp.Answer)
	}
}

func TestTheLeaseIsRenewedWhileAJobRuns(t *testing.T) {
	f := newWorkerFixture(t)
	var w *Worker
	f.provider = llm.ProviderFunc(func(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
		first := f.spy.firstClaim()
		orig := first.ExpiresAt
		f.clk.Advance(time.Second)
		deadline := time.After(wait)
		for {
			select {
			case c := <-f.spy.renewCh:
				if c.ExpiresAt.After(orig) {
					if c.Fence != first.Fence || c.Worker != "w1" {
						t.Errorf("renewed %+v", c)
					}
					if n := w.live.Load(); n != 1 {
						t.Errorf("%d renewal goroutines while the job runs", n)
					}
					return llmtest.Say("done", 1, 1).Resp, nil
				}
			case <-deadline:
				return llm.CompletionResponse{}, errors.New("the lease was not renewed")
			}
		}
	})
	id := f.enqueue("g")
	w = f.worker("w1")
	f.start(w)
	f.expect(id, "released")
	if j := f.job(); j.State != store.JobSucceeded {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
}

func TestALostLeaseCancelsTheRunningJob(t *testing.T) {
	f := newWorkerFixture(t)
	b := newBlocking()
	f.provider = b
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	waitFor(t, b.entered, "the model call")
	old := f.spy.firstClaim()

	// another worker takes over after the lease expired; one transaction, so
	// no renewal slips between the clock advance and the reap
	var c2 store.Claim
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		f.clk.Advance(3 * time.Minute)
		if _, err := tx.ReapExpired(100, func(int) time.Duration { return 0 }); err != nil {
			return err
		}
		var ok bool
		var err error
		if c2, ok, err = tx.ClaimNextJob("w2", 2*time.Minute); err == nil && !ok {
			err = errors.New("nothing to take over")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case cause := <-b.cause:
		if !errors.Is(cause, errLeaseLost) {
			t.Errorf("cause = %v, want errLeaseLost", cause)
		}
	case <-time.After(wait):
		t.Fatal("the model call was not cancelled")
	}
	f.expect(id, "lease_lost")
	if n := f.spy.releasesOf(old.Fence); n != 0 {
		t.Errorf("%d releases of the old claim", n)
	}
	j := f.job()
	if j.State != store.JobRunning || j.Error != "" || len(f.steps()) != 0 || len(f.auditOf("job_finished")) != 0 {
		t.Errorf("the old claim wrote: job %s %q, steps %d, audit %s", j.State, j.Error, len(f.steps()), f.auditKinds())
	}
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.RenewLease(c2, time.Minute)
		return err
	}); err != nil {
		t.Errorf("the new holder lost its lease: %v", err)
	}
}

func TestShutdownRequeuesTheJob(t *testing.T) {
	f := newWorkerFixture(t)
	b := newBlocking()
	f.provider = b
	id := f.enqueue("g")
	stop := f.start(f.worker("w1"))
	waitFor(t, b.entered, "the model call")
	stop()
	f.expect(id, "shutdown")
	// not before the approvals' retry settle window: an approved write cut
	// off by the shutdown is reconciled, not reported unknown
	assertRequeued(t, f.fixture, 2*time.Minute)
	j := f.job()
	if j.Runs != 1 || j.Attempt != 0 {
		t.Errorf("runs %d attempt %d, want 1 and a refunded attempt", j.Runs, j.Attempt)
	}
	if len(f.steps()) != 0 {
		t.Error("no step for a shutdown")
	}
}

func TestTheShutdownRequeueDelayIsConfigurable(t *testing.T) {
	f := newWorkerFixture(t)
	f.cfg.ShutdownRequeueDelay = 3 * time.Minute
	b := newBlocking()
	f.provider = b
	id := f.enqueue("g")
	stop := f.start(f.worker("w1"))
	waitFor(t, b.entered, "the model call")
	stop()
	f.expect(id, "shutdown")
	assertRequeued(t, f.fixture, 3*time.Minute)
}

func TestAPanicInTheRunnerRequeuesTheJob(t *testing.T) {
	f := newWorkerFixture(t)
	f.provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		panic("boom")
	})
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	f.expect(id, "panic")
	assertRequeued(t, f.fixture, 7*time.Second)
}

func TestRepeatedFailuresStopAtTheRunsCap(t *testing.T) {
	f := newWorkerFixture(t)
	f.cfg.MaxRuns = 3
	var calls atomic.Int32
	f.provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		calls.Add(1)
		panic("boom")
	})
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	for _, delay := range []time.Duration{7 * time.Second, 14 * time.Second} {
		f.expect(id, "panic")
		assertRequeued(t, f.fixture, delay)
		f.clk.Advance(time.Hour)
		f.q.Wake()
	}
	f.expect(id, "panic")
	assertFailed(t, f.fixture, ReasonInternal)
	if n := calls.Load(); n != 3 {
		t.Errorf("%d model calls, want 3", n)
	}
	if j := f.job(); j.Runs != 3 {
		t.Errorf("runs = %d", j.Runs)
	}
}

func TestTheKillSwitchStopsClaiming(t *testing.T) {
	t.Run("system key", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.provider = llmtest.New(llmtest.Say("done", 1, 1))
		if err := SetKillSwitch(bg, f.st, true); err != nil {
			t.Fatal(err)
		}
		id := f.enqueue("g")
		w := f.worker("w1")
		checks := make(chan struct{}, 1024)
		w.onKillSwitch = func() {
			select {
			case checks <- struct{}{}:
			default:
			}
		}
		f.start(w)
		for range 3 {
			waitFor(t, checks, "a kill switch check")
		}
		if n := f.spy.claims(); n != 0 {
			t.Fatalf("%d claims with the kill switch on", n)
		}
		if j := f.job(); j.State != store.JobQueued || j.Runs != 0 {
			t.Fatalf("job = %s runs %d", j.State, j.Runs)
		}
		if err := SetKillSwitch(bg, f.st, false); err != nil {
			t.Fatal(err)
		}
		f.expect(id, "released")
		if j := f.job(); j.State != store.JobSucceeded {
			t.Errorf("job = %s", j.State)
		}
	})
	t.Run("env flag", func(t *testing.T) {
		f := newWorkerFixture(t)
		f.cfg.KillSwitch = true
		f.provider = llmtest.New()
		f.enqueue("g")
		w := f.worker("w1")
		checks := make(chan struct{}, 1024)
		w.onKillSwitch = func() {
			select {
			case checks <- struct{}{}:
			default:
			}
		}
		f.start(w)
		for range 3 {
			waitFor(t, checks, "a kill switch check")
		}
		if n := f.spy.claims(); n != 0 {
			t.Fatalf("%d claims with the kill switch on", n)
		}
	})
}

func TestAPausedUsersJobIsNotClaimed(t *testing.T) {
	f := newWorkerFixture(t)
	p := llmtest.New(llmtest.Say("done", 1, 1))
	f.provider = p
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.PausePolicy(userID, store.PauseUser)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	for range 3 {
		waitFor(t, f.spy.emptyCh, "an empty claim")
	}
	if j := f.job(); j.State != store.JobQueued || j.Runs != 0 || len(p.Requests()) != 0 {
		t.Fatalf("job = %s runs %d, %d model calls", j.State, j.Runs, len(p.Requests()))
	}
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.ClearPause(userID, store.PauseUser)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.q.Wake()
	f.expect(id, "released")
}

func TestTwoWorkersNeverRunTheSameUsersJobsConcurrently(t *testing.T) {
	f := newWorkerFixture(t)
	var inflight, peak atomic.Int32
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	f.provider = llm.ProviderFunc(func(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return llm.CompletionResponse{}, ctx.Err()
		}
		return llmtest.Say("done", 1, 1).Resp, nil
	})
	a, b := f.enqueue("a"), f.enqueue("b")
	f.start(f.worker("w1"), f.worker("w2"))
	waitFor(t, entered, "the first model call")
	// drain the empty claims so far, then see the other worker try again
	for len(f.spy.emptyCh) > 0 {
		<-f.spy.emptyCh
	}
	for range 5 {
		waitFor(t, f.spy.emptyCh, "another worker's empty claim")
	}
	if n := f.spy.claims(); n != 1 {
		t.Fatalf("%d claims while the user's first job runs", n)
	}
	close(release)
	got := map[string]string{}
	for range 2 {
		e := f.next()
		got[e.jobID] = e.outcome
	}
	if got[a] != "released" || got[b] != "released" {
		t.Fatalf("processed %v", got)
	}
	if p := peak.Load(); p != 1 {
		t.Errorf("%d jobs of one user in flight at once", p)
	}
}

func TestWorkerLogsDoNotContainProviderErrorText(t *testing.T) {
	f := newWorkerFixture(t)
	var mode atomic.Int32 // 0 panic, 1 block until cancelled
	b := newBlocking()
	f.provider = llm.ProviderFunc(func(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
		if mode.Load() == 0 {
			panic("SECRET-panic-value")
		}
		return b.Complete(ctx, req)
	})
	// a runner error that carries oCIS text
	f.creds.setErr(errors.New("SECRET-ocis-text"))
	j1 := f.enqueue("SECRET-goal-1")
	stop := f.start(f.worker("w1"))
	f.expect(j1, "runner_error")
	f.creds.setErr(nil)

	j2 := f.enqueue("SECRET-goal-2")
	f.expect(j2, "panic")

	mode.Store(1)
	j3 := f.enqueue("SECRET-goal-3")
	waitFor(t, b.entered, "the model call")
	stop()
	f.expect(j3, "shutdown")

	out := f.log.String()
	if strings.Contains(out, "SECRET") {
		t.Errorf("the log carries error or job text:\n%s", out)
	}
	for _, want := range []string{j1, j2, j3, "runner_error", "panic", "shutdown"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
}

func TestTheRenewalPeriodDerivesFromTheQueuesTTL(t *testing.T) {
	f := newWorkerFixture(t)
	f.provider = llmtest.New()
	r := f.runner()
	for _, c := range []struct{ ttl, want time.Duration }{
		{90 * time.Second, 30 * time.Second},
		{workerTTL, workerTTL / 3},
		{time.Millisecond, time.Millisecond}, // never a zero ticker
	} {
		w, err := NewWorker(queue.New(f.st, queue.WithLeaseTTL(c.ttl)), f.st, r, WorkerConfig{ID: "w1"})
		if err != nil {
			t.Fatal(err)
		}
		if w.period != c.want {
			t.Errorf("ttl %s: period %s, want %s", c.ttl, w.period, c.want)
		}
	}
}

func TestATransientRenewErrorDoesNotCancelTheJob(t *testing.T) {
	f := newWorkerFixture(t)
	f.spy.renewFail.Store(1)
	f.provider = llm.ProviderFunc(func(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
		select { // the failed renewal
		case <-f.spy.failCh:
		case <-ctx.Done():
			return llm.CompletionResponse{}, ctx.Err()
		case <-time.After(wait):
			return llm.CompletionResponse{}, errors.New("no failed renewal")
		}
		select { // a successful one after it
		case <-f.spy.renewCh:
		case <-ctx.Done():
			return llm.CompletionResponse{}, ctx.Err()
		case <-time.After(wait):
			return llm.CompletionResponse{}, errors.New("no renewal after the failure")
		}
		return llmtest.Say("done", 1, 1).Resp, nil
	})
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	f.expect(id, "released")
	if j := f.job(); j.State != store.JobSucceeded {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if !strings.Contains(f.log.String(), "outcome=renew_error") {
		t.Errorf("the failed renewal is not logged:\n%s", f.log.String())
	}
}

func TestAJobCancelledInTheStoreCancelsTheRun(t *testing.T) {
	f := newWorkerFixture(t)
	b := newBlocking()
	f.provider = b
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	waitFor(t, b.entered, "the model call")
	claim := f.spy.firstClaim()
	if err := f.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, id) }); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-b.cause:
		if !errors.Is(cause, errLeaseLost) {
			t.Errorf("cause = %v, want errLeaseLost", cause)
		}
	case <-time.After(wait):
		t.Fatal("the model call was not cancelled")
	}
	f.expect(id, "lease_lost")
	if n := f.spy.releasesOf(claim.Fence); n != 0 {
		t.Errorf("%d releases after the cancel", n)
	}
	j := f.job()
	if j.State != store.JobCancelled || j.Error != "" || len(f.steps()) != 0 || len(f.auditOf("job_finished")) != 0 {
		t.Errorf("written after the cancel: job %s %q, steps %d, audit %s", j.State, j.Error, len(f.steps()), f.auditKinds())
	}
}

func TestTheLeaseNeverExpiresWhileAJobRuns(t *testing.T) {
	f := newWorkerFixture(t)
	f.clk.Follow() // the store's lease clock moves in real time
	var reaped atomic.Int32
	f.provider = llm.ProviderFunc(func(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
		end := time.After(4 * workerTTL)
		for {
			select {
			case <-end:
				return llmtest.Say("done", 1, 1).Resp, nil
			case <-ctx.Done():
				return llm.CompletionResponse{}, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
			res, err := f.q.Reap(bg, 100)
			if err != nil {
				return llm.CompletionResponse{}, err
			}
			reaped.Add(int32(res.Requeued + res.Failed))
		}
	})
	id := f.enqueue("g")
	f.start(f.worker("w1"))
	e := f.next()
	if n := reaped.Load(); n != 0 {
		t.Fatalf("the lease expired %d times while the job ran", n)
	}
	if e.jobID != id || e.outcome != "released" || f.job().State != store.JobSucceeded {
		t.Fatalf("processed %+v, job %s", e, f.job().State)
	}
	if n := f.spy.renewals(); n < 3 {
		t.Errorf("%d renewals in 4 ttls", n)
	}
}

func TestGoexitInTheRunnerStopsTheRenewal(t *testing.T) {
	f := newWorkerFixture(t)
	f.provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		runtime.Goexit()
		return llm.CompletionResponse{}, nil
	})
	f.enqueue("g")
	w := f.worker("w1")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("Run did not end with the Goexit")
	}
	// the parent ctx is still alive: only the deferred cleanup stops it
	if n := w.live.Load(); n != 0 {
		t.Errorf("%d renewal goroutines left after Goexit", n)
	}
}

func TestAKillSwitchTurnedOnWhileClaimBlocksStopsTheJobAndTheWorker(t *testing.T) {
	f := newWorkerFixture(t)
	p := llmtest.New()
	f.provider = p
	w := f.worker("w1")
	checks := make(chan struct{}, 1024)
	w.onKillSwitch = func() {
		select {
		case checks <- struct{}{}:
		default:
		}
	}
	f.start(w)
	waitFor(t, f.spy.emptyCh, "a blocked claim")
	if err := SetKillSwitch(bg, f.st, true); err != nil {
		t.Fatal(err)
	}
	id := f.enqueue("g")
	f.expect(id, "released")
	assertRequeued(t, f.fixture, DefaultConfig().PausedDelay)
	if j := f.job(); j.Runs != 1 || len(p.Requests()) != 0 {
		t.Errorf("runs %d, %d model calls", j.Runs, len(p.Requests()))
	}
	for range 3 {
		waitFor(t, checks, "a kill switch check")
	}
	if n := f.spy.claims(); n != 1 {
		t.Errorf("%d claims, want 1", n)
	}
}
