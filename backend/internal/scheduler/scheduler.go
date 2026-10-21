// Package scheduler runs the periodic maintenance of the sidecar: reaping
// expired job leases, expiring and settling approvals, reconciling orphaned
// calls, renewing and sweeping app tokens and pausing dormant users. It also
// starts the scheduled proactive sweeps (proactive.go), which, unlike the
// maintenance, never start anything while the kill switch is on or a user
// is paused.
//
// Every task has its own interval and runs from Tick when it is due. Tick is
// synchronous and reads time only through Deps.Now, so tests drive it with a
// fake clock; Run calls Tick on a ticker. Tasks run regardless of the kill
// switch and of user pauses: they keep the state consistent and the
// credentials alive while the agent does no work. A task that fails or
// panics is reported and does not stop the others. The tasks that call oCIS
// (reconcile, renew, sweep) run last, each under Config.TaskTimeout, and
// renew and sweep stop their batch at the first item that finds oCIS
// unavailable; the rest is retried at the next run. Logs carry task names,
// counts and fixed categories only, never error text (errors from oCIS, the
// store or a tool may carry paths or values that must not reach the log).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Task names, in the order Tick runs them.
const (
	TaskReap      = "reap"
	TaskExpire    = "expire"
	TaskSettle    = "settle"
	TaskDormant   = "dormant"
	TaskProactive = "proactive"
	TaskReconcile = "reconcile"
	TaskRenew     = "renew"
	TaskSweep     = "sweep"
)

// Failure categories reported in TickResult.Failed and in the logs.
const (
	CategoryInternal     = "internal"
	CategoryUnavailable  = "unavailable"
	CategoryUnauthorized = "unauthorized"
	CategoryPanic        = "panic"
)

// Queue requeues or fails jobs whose lease expired and wakes the workers
// after the proactive task started jobs (*queue.Queue).
type Queue interface {
	Reap(ctx context.Context, maxAttempts int) (store.ReapResult, error)
	Wake()
}

// Settler expires approvals (*approvals.Gate).
type Settler interface {
	ExpireAndResume(ctx context.Context) (int, error)
	SettleEnded(ctx context.Context, limit int) (int, error)
}

// Reconciler resolves the open calls of ended jobs (*approvals.Executor).
type Reconciler interface {
	ReconcileOrphans(ctx context.Context, envFor approvals.EnvFunc, olderThan time.Time, limit int) (approvals.ReconcileSummary, error)
}

// Auth is the credential maintenance the scheduler drives (*auth.Service).
// The scheduler renews the due users one by one, so it can stop at an outage.
type Auth interface {
	DueForRenewal(ctx context.Context, limit int) ([]string, error)
	Renew(ctx context.Context, userID string) (auth.Renewed, error)
	Sweep(ctx context.Context, userID string) (auth.SweepResult, error)
	PauseDormant(ctx context.Context, limit int) (int, error)
}

// Deps are the scheduler's collaborators; all are required.
type Deps struct {
	Store    store.Store
	Queue    Queue
	Gate     Settler
	Executor Reconciler
	Env      approvals.EnvFunc
	Auth     Auth
	Log      *slog.Logger
	Now      func() time.Time
}

// Intervals are the cadences of the tasks. Zero means the default.
type Intervals struct {
	Reap      time.Duration // default LeaseTTL/2, at least 1s
	Expire    time.Duration // default 1m
	Settle    time.Duration // default 1m
	Reconcile time.Duration // default 1m
	Renew     time.Duration // default 1m
	Sweep     time.Duration // one page of users per run; default 10s
	Dormant   time.Duration // default 1h
	Proactive time.Duration // default 1m
	Tick      time.Duration // how often Run calls Tick; default 1s
}

// Config holds the scheduler settings. Zero values take the defaults;
// negative values are refused.
type Config struct {
	MaxAttempts   int           // crash attempts per job before the reaper fails it (default 3)
	LeaseTTL      time.Duration // job lease duration (default 2m)
	RetrySettle   time.Duration // approvals.Config.RetrySettle (default 2m)
	CallTimeout   time.Duration // the longest a tool call can take (default 5m)
	SweepInterval time.Duration // how often each user's tokens are swept (default 15m)
	TaskTimeout   time.Duration // deadline of one run of reconcile, renew and sweep (default 2m)
	Intervals     Intervals
	Batch         int // items per task run (default 100, at most 1000)
	Proactive     ProactiveConfig
}

const maxBatch = 1000

func (c Config) withDefaults() (Config, error) {
	if c.MaxAttempts < 0 || c.LeaseTTL < 0 || c.RetrySettle < 0 || c.CallTimeout < 0 || c.SweepInterval < 0 || c.TaskTimeout < 0 || c.Batch < 0 ||
		c.Proactive.PerTick < 0 || c.Proactive.Limits.DailyJobs < 0 || c.Proactive.Limits.DailyTokens < 0 {
		return c, errors.New("scheduler: negative config value")
	}
	if c.Batch > maxBatch {
		return c, fmt.Errorf("scheduler: batch above %d", maxBatch)
	}
	iv := &c.Intervals
	for _, d := range []time.Duration{iv.Reap, iv.Expire, iv.Settle, iv.Reconcile, iv.Renew, iv.Sweep, iv.Dormant, iv.Proactive, iv.Tick} {
		if d < 0 {
			return c, errors.New("scheduler: negative interval")
		}
	}
	def := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.Batch == 0 {
		c.Batch = 100
	}
	if c.Proactive.PerTick == 0 {
		c.Proactive.PerTick = 50
	}
	if c.Proactive.Limits.DailyJobs == 0 {
		c.Proactive.Limits.DailyJobs = 24
	}
	def(&c.LeaseTTL, 2*time.Minute)
	def(&c.RetrySettle, 2*time.Minute)
	def(&c.CallTimeout, 5*time.Minute)
	def(&c.SweepInterval, 15*time.Minute)
	def(&c.TaskTimeout, 2*time.Minute)
	def(&iv.Reap, max(c.LeaseTTL/2, time.Second))
	def(&iv.Expire, time.Minute)
	def(&iv.Settle, time.Minute)
	def(&iv.Reconcile, time.Minute)
	def(&iv.Renew, time.Minute)
	def(&iv.Sweep, 10*time.Second)
	def(&iv.Dormant, time.Hour)
	def(&iv.Proactive, time.Minute)
	def(&iv.Tick, time.Second)
	return c, nil
}

// TickResult reports what one Tick did.
type TickResult struct {
	Ran    []string          // tasks that were due and ran, in run order
	Counts map[string]int    // per task: items it acted on
	Errors map[string]int    // per task: item errors it reported (reconcile, renew, sweep)
	Failed map[string]string // per failed task: its category
}

type task struct {
	name     string
	interval time.Duration
	ocis     bool // talks to oCIS: runs under Config.TaskTimeout
	run      func(ctx context.Context, now time.Time) (count, errs int, err error)
}

// Scheduler runs the maintenance tasks. Tick and Run must not be called
// concurrently.
type Scheduler struct {
	d     Deps
	c     Config
	tasks []task
	last  map[string]time.Time
	sweep sweepState
	pro   proactiveState
}

// New validates the dependencies and the configuration.
func New(d Deps, c Config) (*Scheduler, error) {
	if d.Store == nil || d.Queue == nil || d.Gate == nil || d.Executor == nil || d.Env == nil || d.Auth == nil || d.Log == nil || d.Now == nil {
		return nil, errors.New("scheduler: every dependency is required")
	}
	c, err := c.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &Scheduler{d: d, c: c, last: map[string]time.Time{}, sweep: newSweepState(), pro: newProactiveState()}
	iv := c.Intervals
	// The database-only tasks run first, so a slow or hung oCIS delays them
	// by at most the three TaskTimeouts of the previous Tick.
	s.tasks = []task{
		{TaskReap, iv.Reap, false, s.reap},
		{TaskExpire, iv.Expire, false, s.expire},
		{TaskSettle, iv.Settle, false, s.settle},
		{TaskDormant, iv.Dormant, false, s.dormant},
		{TaskProactive, iv.Proactive, false, s.proactive},
		{TaskReconcile, iv.Reconcile, true, s.reconcile},
		{TaskRenew, iv.Renew, true, s.renew},
		{TaskSweep, iv.Sweep, true, s.sweepRun},
	}
	return s, nil
}

// Tick runs every task that is due (the first Tick runs them all). A task's
// interval counts from the start of its last run, whether it succeeded or
// not. Once ctx ends no further task starts.
func (s *Scheduler) Tick(ctx context.Context) TickResult {
	res := TickResult{Counts: map[string]int{}, Errors: map[string]int{}, Failed: map[string]string{}}
	for _, t := range s.tasks {
		if ctx.Err() != nil {
			break
		}
		now := s.d.Now()
		if last, ok := s.last[t.name]; ok && now.Sub(last) < t.interval {
			continue
		}
		s.last[t.name] = now
		res.Ran = append(res.Ran, t.name)
		count, errs, cat := s.runTask(ctx, t, now)
		res.Counts[t.name] = count
		if errs > 0 {
			res.Errors[t.name] = errs
		}
		if cat != "" {
			res.Failed[t.name] = cat
			s.d.Log.Warn("scheduler: task failed", "task", t.name, "category", cat)
		} else if count > 0 || errs > 0 {
			s.d.Log.Debug("scheduler: task ran", "task", t.name, "count", count, "errors", errs)
		}
	}
	return res
}

// runTask runs one task under recover. It returns the task's counts and, if
// it failed, the failure category. A panic value is never logged.
func (s *Scheduler) runTask(ctx context.Context, t task, now time.Time) (count, errs int, cat string) {
	defer func() {
		if recover() != nil {
			cat = CategoryPanic
		}
	}()
	if t.ocis {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.c.TaskTimeout)
		defer cancel()
	}
	count, errs, err := t.run(ctx, now)
	if err != nil {
		return count, errs, category(err)
	}
	return count, errs, ""
}

// Run calls Tick at once and then every Intervals.Tick until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.c.Intervals.Tick)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
