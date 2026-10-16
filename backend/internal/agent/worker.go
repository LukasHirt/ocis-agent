package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	// killSwitchPoll is how often a worker re-reads a kill switch that is on.
	killSwitchPoll = 2 * time.Second
	// shutdownRequeueTimeout bounds the requeue of a job interrupted by
	// shutdown.
	shutdownRequeueTimeout = 5 * time.Second
	// settleTimeout bounds the release after a panic or a runner error.
	settleTimeout = 15 * time.Second
)

// The outcomes of a processed claim, and the only text the worker logs about
// it besides ids: error values may carry model, oCIS or job text and are never
// logged.
const (
	outcomeReleased    = "released" // the runner released the job itself
	outcomeLeaseLost   = "lease_lost"
	outcomeShutdown    = "shutdown"
	outcomePanic       = "panic"
	outcomeRunnerError = "runner_error"
)

var (
	errLeaseLost = errors.New("agent: the job's lease was lost")
	errJobEnded  = errors.New("agent: the job's processing ended")
)

// WorkerConfig configures a Worker. Log defaults to slog.Default.
//
// JobContext, if set, is the context the claimed jobs run under: the context
// given to Run then controls claiming only, so a worker can drain (stop
// claiming, let the running job finish) before its jobs are cancelled. When
// JobContext ends, the running job is cancelled and requeued after
// Config.ShutdownRequeueDelay, and the worker stops claiming too. Unset, the
// jobs run under the context given to Run.
type WorkerConfig struct {
	ID         string
	Log        *slog.Logger
	JobContext context.Context
}

// Worker claims jobs one at a time and runs them with the Runner, keeping the
// lease alive while a job runs.
type Worker struct {
	q   *queue.Queue
	st  store.Store
	r   *Runner
	id  string
	log *slog.Logger
	// jobBase is WorkerConfig.JobContext (nil: the Run context)
	jobBase context.Context

	killPoll time.Duration
	period   time.Duration // lease renewal: a third of the queue's ttl
	live     atomic.Int32  // running renewal goroutines

	// test hooks
	onDone       func(claim store.Claim, outcome string)
	onKillSwitch func()
}

// NewWorker checks the collaborators and the config. The lease is renewed
// every third of the queue's lease ttl.
func NewWorker(q *queue.Queue, st store.Store, r *Runner, cfg WorkerConfig) (*Worker, error) {
	if q == nil || st == nil || r == nil {
		return nil, errors.New("agent: the worker needs a queue, a store and a runner")
	}
	if cfg.ID == "" {
		return nil, errors.New("agent: the worker needs an id")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Worker{
		q: q, st: st, r: r, id: cfg.ID, log: cfg.Log, jobBase: cfg.JobContext,
		killPoll: killSwitchPoll, period: max(q.LeaseTTL()/3, time.Millisecond),
	}, nil
}

// Run claims and processes jobs until ctx (or the JobContext) ends, then
// returns once the job it is running, if any, has been settled. While the kill
// switch is on it claims nothing.
func (w *Worker) Run(ctx context.Context) {
	base := w.jobBase
	if base == nil {
		base = ctx
	}
	// claiming ends with either context: without a job context left, a
	// claimed job could only be requeued again
	ctx, stopClaiming := context.WithCancel(ctx)
	defer stopClaiming()
	defer context.AfterFunc(base, stopClaiming)()
	for ctx.Err() == nil {
		on, err := KillSwitchOn(ctx, w.st, w.r.cfg.KillSwitch)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("agent worker: kill switch unreadable", "worker", w.id, "outcome", "killswitch_error")
			}
			sleep(ctx, w.killPoll)
			continue
		}
		if on {
			if w.onKillSwitch != nil {
				w.onKillSwitch()
			}
			sleep(ctx, w.killPoll)
			continue
		}
		claim, err := w.q.Claim(ctx, w.id)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("agent worker: claim failed", "worker", w.id, "outcome", "claim_error")
				sleep(ctx, w.killPoll)
			}
			continue
		}
		w.process(base, claim)
	}
}

// RunPool runs n workers (nil ones are skipped) until ctx ends and waits for
// all of them. ctx is each worker's Run context, so with a JobContext in the
// workers' config it stops claiming only and RunPool returns once the
// running jobs have finished.
func RunPool(ctx context.Context, n int, newWorker func(i int) *Worker) {
	var wg sync.WaitGroup
	for i := range n {
		w := newWorker(i)
		if w == nil {
			continue
		}
		wg.Go(func() { w.Run(ctx) })
	}
	wg.Wait()
}

// process runs one claim under a context derived from ctx (the job
// context, not the claim context) with its lease renewed, then settles what
// the runner left: the renewal goroutine has ended before process returns.
func (w *Worker) process(ctx context.Context, claim store.Claim) {
	jobCtx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	// deferred too, so a Goexit in the runner cannot leave the renewal running
	defer wg.Wait()
	defer cancel(errJobEnded)
	w.live.Add(1)
	wg.Go(func() {
		defer w.live.Add(-1)
		w.renew(jobCtx, cancel, claim)
	})
	panicked, err := w.runJob(jobCtx, claim)
	cancel(errJobEnded)
	wg.Wait()

	outcome := w.settle(ctx, jobCtx, claim, panicked, err)
	level := slog.LevelInfo
	if outcome == outcomePanic || outcome == outcomeRunnerError {
		level = slog.LevelWarn
	}
	w.log.Log(ctx, level, "agent worker: claim processed", "worker", w.id, "job_id", claim.Job.ID, "outcome", outcome)
	if w.onDone != nil {
		w.onDone(claim, outcome)
	}
}

// runJob calls the runner and turns a panic into an outcome. The panic value
// is dropped: it may carry job text.
func (w *Worker) runJob(ctx context.Context, claim store.Claim) (panicked bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			panicked, err = true, nil
		}
	}()
	return false, w.r.Run(ctx, claim)
}

// renew extends the lease every ttl/3 until ctx ends. A lease that is no
// longer ours cancels the job with errLeaseLost; other errors are retried at
// the next tick, but once no renewal has succeeded for a whole ttl (counted
// from the claim) the lease may have expired, so the job is cancelled with
// errLeaseLost too.
func (w *Worker) renew(ctx context.Context, cancel context.CancelCauseFunc, claim store.Claim) {
	t := time.NewTicker(w.period)
	defer t.Stop()
	ttl := w.q.LeaseTTL()
	lastOK := time.Now() // the claim was just made
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := w.q.Renew(ctx, claim)
		switch {
		case err == nil:
			lastOK = time.Now()
		case lost(err):
			cancel(errLeaseLost)
			return
		case ctx.Err() != nil:
			return
		default:
			w.log.Warn("agent worker: lease renewal failed", "worker", w.id, "job_id", claim.Job.ID, "outcome", "renew_error")
			if time.Since(lastOK) >= ttl {
				cancel(errLeaseLost)
				return
			}
		}
	}
}

// settle decides the job's fate after the runner returned, through the same
// fenced store writes the runner uses, and reports the outcome. ctx is the
// job context: its end is a shutdown, whatever happened to claiming.
func (w *Worker) settle(ctx, jobCtx context.Context, claim store.Claim, panicked bool, err error) string {
	switch {
	case !panicked && err == nil:
		return outcomeReleased
	case lost(err) || errors.Is(context.Cause(jobCtx), errLeaseLost):
		// the job belongs to someone else or ended meanwhile
		return outcomeLeaseLost
	case ctx.Err() != nil:
		rctx, done := context.WithTimeout(context.WithoutCancel(ctx), shutdownRequeueTimeout)
		defer done()
		if err := w.r.requeue(rctx, claim, w.r.cfg.ShutdownRequeueDelay); err != nil {
			w.log.Warn("agent worker: shutdown requeue failed", "worker", w.id, "job_id", claim.Job.ID, "outcome", "release_failed")
		}
		return outcomeShutdown
	}
	outcome := outcomeRunnerError
	if panicked {
		outcome = outcomePanic
	}
	wctx, done := context.WithTimeout(ctx, settleTimeout)
	defer done()
	var rerr error
	if claim.Job.Runs >= w.r.cfg.MaxRuns {
		rerr = w.r.fail(wctx, claim, ReasonInternal)
	} else {
		rerr = w.r.backoff(wctx, claim)
	}
	if rerr != nil {
		w.log.Warn("agent worker: release failed", "worker", w.id, "job_id", claim.Job.ID, "outcome", "release_failed")
	}
	return outcome
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
