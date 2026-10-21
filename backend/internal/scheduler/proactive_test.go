package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// enableProactive turns proactive work on for user with a one-hour sweep
// interval and a last sweep lastAgo before now (no cursor when lastAgo < 0).
func (fx *fixture) enableProactive(user string, lastAgo time.Duration, mod ...func(*store.Policy)) {
	fx.t.Helper()
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		p, err := tx.GetPolicy(user)
		if err != nil {
			return err
		}
		p.ProactiveEnabled, p.SweepInterval = true, time.Hour
		for _, m := range mod {
			m(&p)
		}
		if err := tx.PutPolicy(p); err != nil {
			return err
		}
		if lastAgo < 0 {
			return nil
		}
		c, err := tx.GetCursor(user)
		if err != nil {
			return err
		}
		at := fx.clk.Now().Add(-lastAgo)
		c.LastSweepAt = &at
		return tx.PutCursor(c)
	}); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) jobsOf(user string) []store.Job {
	fx.t.Helper()
	var jobs []store.Job
	if err := fx.st.View(bg, func(tx store.Tx) error {
		var err error
		jobs, err = tx.ListJobs(user, store.JobFilter{})
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	return jobs
}

func (fx *fixture) lastSweep(user string) *time.Time {
	fx.t.Helper()
	var c store.Cursor
	if err := fx.st.View(bg, func(tx store.Tx) error {
		var err error
		c, err = tx.GetCursor(user)
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	return c.LastSweepAt
}

// auditOf returns the user's audit events of kind as "outcome detail".
func (fx *fixture) auditOf(user, kind string) []string {
	fx.t.Helper()
	var recs []audit.Record
	if err := fx.st.View(bg, func(tx store.Tx) error {
		var err error
		recs, err = tx.ReadAudit(user, 0, 1000)
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Kind == kind {
			out = append(out, r.Outcome+" "+r.Detail)
		}
	}
	return out
}

// insertJob adds a job of kind for user (and ends it in state if that is
// terminal).
func (fx *fixture) insertJob(user, kind string, state store.JobState) {
	fx.t.Helper()
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		j, err := tx.InsertJob(store.Job{UserID: user, TriggerKind: kind, Goal: "g"})
		if err != nil || state == store.JobQueued {
			return err
		}
		return tx.CancelJob(user, j.ID)
	}); err != nil {
		fx.t.Fatal(err)
	}
}

// useTokens records a finished chat job of user that used n tokens.
func (fx *fixture) useTokens(user string, n int64) {
	fx.t.Helper()
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.InsertJob(store.Job{UserID: user, TriggerKind: store.TriggerChat, Goal: "g"}); err != nil {
			return err
		}
		c, ok, err := tx.ClaimNextJob("w", time.Minute)
		if err != nil || !ok {
			return errors.New("claim failed")
		}
		if _, err := tx.AppendStep(store.JobStep{JobID: c.Job.ID, Kind: "llm", Status: "ok", TokensIn: n}, c.Fence); err != nil {
			return err
		}
		_, err = tx.ReleaseJob(c, store.JobSucceeded, nil, "")
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) runProactive(s *Scheduler) (int, int) {
	fx.t.Helper()
	n, errs, err := s.proactive(bg, fx.clk.Now())
	if err != nil {
		fx.t.Fatalf("proactive: %v", err)
	}
	return n, errs
}

var reviewGoal, _ = store.ProactiveGoal(store.TriggerSchedule)

func TestProactiveFirstSweepComesOneIntervalAfterEnabling(t *testing.T) {
	fx := newFixture(t)
	u := fx.addUsers(1)[0]
	fx.enableProactive(u, -1)
	s := fx.scheduler()

	if n, _ := fx.runProactive(s); n != 0 || len(fx.jobsOf(u)) != 0 {
		t.Fatalf("enabling must not start a sweep at once: %d started", n)
	}
	base := fx.lastSweep(u)
	if base == nil || !base.Equal(fx.clk.Now()) {
		t.Fatalf("baseline = %v, want now", base)
	}
	fx.clk.Advance(59 * time.Minute)
	if n, _ := fx.runProactive(s); n != 0 {
		t.Fatalf("started %d before the interval passed", n)
	}
	fx.clk.Advance(time.Minute)
	woken := fx.f.wakes()
	if n, _ := fx.runProactive(s); n != 1 {
		t.Fatalf("started %d at the interval, want 1", n)
	}
	jobs := fx.jobsOf(u)
	if len(jobs) != 1 || jobs[0].TriggerKind != store.TriggerSchedule || jobs[0].TriggerRef != "" ||
		jobs[0].Goal != reviewGoal || jobs[0].State != store.JobQueued {
		t.Fatalf("jobs = %+v", jobs)
	}
	if got := fx.lastSweep(u); got == nil || !got.Equal(fx.clk.Now()) {
		t.Errorf("last sweep = %v, want now", got)
	}
	if got := fx.auditOf(u, audit.KindProactiveStarted); !slices.Equal(got, []string{"queued trigger=schedule"}) {
		t.Errorf("audit = %v", got)
	}
	if fx.f.wakes() <= woken {
		t.Error("the queue was not woken")
	}
	// the next one comes an interval later, not at the next tick
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(s); n != 0 {
		t.Errorf("started %d a minute later", n)
	}
}

func TestProactiveDueSelection(t *testing.T) {
	type setup func(fx *fixture, u string)
	pausedFor := func(reason string) setup {
		return func(fx *fixture, u string) {
			if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(u, reason); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}
	quiet := func(q string) setup {
		return func(fx *fixture, u string) {
			fx.enableProactive(u, time.Hour, func(p *store.Policy) { p.QuietHours = q })
		}
	}
	// the fixture clock is 12:00 UTC; the budget is 1000 tokens
	cases := []struct {
		name    string
		lastAgo time.Duration
		setup   setup
		skip    string // "" with start: a job; "" without start: silently not due
		start   bool
	}{
		{"due", time.Hour, nil, "", true},
		{"long overdue", 30 * 24 * time.Hour, nil, "", true},
		{"interval not passed", time.Hour - time.Second, nil, "", false},
		{"disabled", time.Hour, func(fx *fixture, u string) {
			fx.enableProactive(u, time.Hour, func(p *store.Policy) { p.ProactiveEnabled = false })
		}, "", false},
		{"paused by the user", time.Hour, pausedFor(store.PauseUser), store.SkipPaused, false},
		{"dormant", time.Hour, pausedFor(store.PauseDormant), store.SkipPaused, false},
		{"token anomaly", time.Hour, pausedFor(store.PauseTokenAnomaly), store.SkipPaused, false},
		{"credentials invalid", time.Hour, pausedFor(store.PauseCredentialInvalid), store.SkipPaused, false},
		{"credentials revoked", time.Hour, pausedFor(store.PauseCredentialRevoked), store.SkipPaused, false},
		{"kill switch in the database", time.Hour, func(fx *fixture, _ string) {
			if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) }); err != nil {
				t.Fatal(err)
			}
		}, "", false},
		{"kill switch in the environment", time.Hour, func(fx *fixture, _ string) { fx.cfg.Proactive.Limits.KillSwitchEnv = true }, "", false},
		{"quiet hours wrapping midnight", time.Hour, quiet("22:00-12:01"), store.SkipQuietHours, false},
		{"quiet hours starting now", time.Hour, quiet("12:00-13:00"), store.SkipQuietHours, false},
		{"quiet hours ending now", time.Hour, quiet("11:00-12:00"), "", true},
		{"quiet hours at night", time.Hour, quiet("22:00-07:00"), "", true},
		{"budget 79%", time.Hour, func(fx *fixture, u string) { fx.useTokens(u, 790) }, "", true},
		{"budget 80%", time.Hour, func(fx *fixture, u string) { fx.useTokens(u, 800) }, store.SkipBudget, false},
		{"budget 81%", time.Hour, func(fx *fixture, u string) { fx.useTokens(u, 810) }, store.SkipBudget, false},
		{"a live proactive job", time.Hour, func(fx *fixture, u string) { fx.insertJob(u, store.TriggerManual, store.JobQueued) }, store.SkipLiveJob, false},
		{"a live file event job", time.Hour, func(fx *fixture, u string) { fx.insertJob(u, store.TriggerFileEvent, store.JobQueued) }, store.SkipLiveJob, false},
		{"a live chat job does not count", time.Hour, func(fx *fixture, u string) { fx.insertJob(u, store.TriggerChat, store.JobQueued) }, "", true},
		{"one below the daily cap", time.Hour, func(fx *fixture, u string) {
			fx.insertJob(u, store.TriggerManual, store.JobCancelled)
		}, "", true},
		{"daily cap", time.Hour, func(fx *fixture, u string) {
			fx.insertJob(u, store.TriggerManual, store.JobCancelled)
			fx.insertJob(u, store.TriggerFileEvent, store.JobCancelled)
		}, store.SkipDailyCap, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.cfg.Proactive.Limits = store.ProactiveLimits{DailyJobs: 2, DailyTokens: 1000}
			u := fx.addUsers(1)[0]
			fx.enableProactive(u, c.lastAgo)
			if c.setup != nil {
				c.setup(fx, u)
			}
			before := fx.lastSweep(u)
			jobsBefore := len(fx.jobsOf(u))
			n, errs := fx.runProactive(fx.scheduler())
			if errs != 0 {
				t.Fatalf("%d item errors", errs)
			}
			started := len(fx.jobsOf(u)) - jobsBefore
			if c.start != (n == 1 && started == 1) {
				t.Fatalf("started %d (count %d), want start=%v", started, n, c.start)
			}
			skips := fx.auditOf(u, audit.KindProactiveSkipped)
			if c.skip != "" {
				if want := []string{"skipped trigger=schedule reason=" + c.skip}; !slices.Equal(skips, want) {
					t.Errorf("skips = %v, want %v", skips, want)
				}
			} else if len(skips) != 0 {
				t.Errorf("skips = %v, want none", skips)
			}
			after := fx.lastSweep(u)
			if c.start {
				if after == nil || !after.Equal(fx.clk.Now()) {
					t.Errorf("last sweep = %v, want now", after)
				}
			} else if (before == nil) != (after == nil) || (before != nil && !before.Equal(*after)) {
				t.Errorf("a skip moved the last sweep from %v to %v", before, after)
			}
		})
	}
}

func TestProactiveSkipsAreAuditedOncePerReasonAndHour(t *testing.T) {
	fx := newFixture(t)
	u := fx.addUsers(1)[0]
	fx.enableProactive(u, time.Hour)
	fx.insertJob(u, store.TriggerManual, store.JobQueued)
	s := fx.scheduler()
	skips := func() int { return len(fx.auditOf(u, audit.KindProactiveSkipped)) }

	fx.runProactive(s)
	for range 59 {
		fx.clk.Advance(time.Minute)
		fx.runProactive(s)
	}
	if got := skips(); got != 1 {
		t.Fatalf("%d skip events within the hour, want 1", got)
	}
	// another reason is recorded at once
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(u, store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(time.Minute)
	fx.runProactive(s)
	if got := fx.auditOf(u, audit.KindProactiveSkipped); len(got) != 2 || !strings.HasSuffix(got[1], "reason=paused") {
		t.Fatalf("skips = %v", got)
	}
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.ClearPause(u, store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	// an hour after the first live_job event the same reason is recorded again
	fx.clk.Advance(time.Minute)
	fx.runProactive(s)
	if got := skips(); got != 3 {
		t.Fatalf("%d skip events after the hour, want 3", got)
	}
	if len(fx.jobsOf(u)) != 1 {
		t.Fatal("a job was started despite the live job")
	}

	// the skip left the sweep due: once the live job ends the next run starts one
	for _, j := range fx.jobsOf(u) {
		if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(u, j.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(s); n != 1 {
		t.Fatalf("started %d after the live job ended, want 1", n)
	}
}

func TestProactiveSkipThrottleForgetsOldEntries(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(3)
	for _, u := range users {
		fx.enableProactive(u, time.Hour)
		if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(u, store.PauseUser); return err }); err != nil {
			t.Fatal(err)
		}
	}
	s := fx.scheduler()
	fx.runProactive(s)
	if got := s.trackedSkips(); got != 3 {
		t.Fatalf("tracked %d skips, want 3", got)
	}
	for _, u := range users[1:] {
		if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.DeleteUser(u) }); err != nil {
			t.Fatal(err)
		}
	}
	fx.clk.Advance(time.Hour)
	fx.runProactive(s)
	if got := s.trackedSkips(); got != 1 {
		t.Fatalf("tracked %d skips after an hour, want only the user still skipped", got)
	}
}

// failStartStore makes every proactive_started audit fail, so the start's
// transaction rolls back after the job was inserted: a crash between
// selection and enqueue.
type failStartStore struct {
	store.Store
	mu    sync.Mutex
	fails int
}

func (s *failStartStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return s.Store.InTx(ctx, func(tx store.Tx) error { return fn(failStartTx{Tx: tx, s: s}) })
}

type failStartTx struct {
	store.Tx
	s *failStartStore
}

func (t failStartTx) AppendAudit(ev audit.Event) (audit.Record, error) {
	if ev.Kind == audit.KindProactiveStarted {
		t.s.mu.Lock()
		t.s.fails++
		t.s.mu.Unlock()
		return audit.Record{}, errors.New("crash")
	}
	return t.Tx.AppendAudit(ev)
}

func TestProactiveStartIsAtomicAndNeverDoubles(t *testing.T) {
	fx := newFixture(t)
	u := fx.addUsers(1)[0]
	fx.enableProactive(u, time.Hour)
	before := fx.lastSweep(u)

	// a crash inside the start's transaction leaves nothing behind
	failing := &failStartStore{Store: fx.st}
	deps := fx.deps
	deps.Store = failing
	crashed, err := New(deps, fx.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, errs, err := crashed.proactive(bg, fx.clk.Now()); err != nil || errs != 1 || failing.fails != 1 {
		t.Fatalf("errs = %d, err = %v, fails = %d", errs, err, failing.fails)
	}
	if len(fx.jobsOf(u)) != 0 || !fx.lastSweep(u).Equal(*before) {
		t.Fatalf("a rolled-back start left a job or moved the mark: %v", fx.jobsOf(u))
	}

	// a restarted process (fresh in-memory state) starts it once
	a, b := fx.scheduler(), fx.scheduler()
	if n, _ := fx.runProactive(a); n != 1 {
		t.Fatalf("started %d, want 1", n)
	}
	if n, _ := fx.runProactive(b); n != 0 {
		t.Fatalf("a second scheduler started %d more", n)
	}
	// a restart right after the commit, even with the job already ended,
	// does not fire again before the interval
	for _, j := range fx.jobsOf(u) {
		if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(u, j.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(fx.scheduler()); n != 0 {
		t.Fatalf("a restart started %d more", n)
	}
	if got := len(fx.jobsOf(u)); got != 1 {
		t.Fatalf("%d jobs, want 1", got)
	}
}

func TestProactiveConcurrentSchedulersStartOnce(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(5)
	for _, u := range users {
		fx.enableProactive(u, time.Hour)
	}
	var wg sync.WaitGroup
	for range 4 {
		s := fx.scheduler()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = s.proactive(bg, fx.clk.Now())
		}()
	}
	wg.Wait()
	for _, u := range users {
		if got := len(fx.jobsOf(u)); got != 1 {
			t.Errorf("%s has %d jobs, want 1", u, got)
		}
	}
}

func TestProactivePerTickLimitAndRotation(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.Batch = 2 // pages of two users
	fx.cfg.Proactive.PerTick = 3
	users := fx.addUsers(7)
	for _, u := range users {
		fx.enableProactive(u, time.Hour)
	}
	// a paused user does not use up the tick's limit
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(users[1], store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	s := fx.scheduler()
	startedBy := func() []string {
		var out []string
		for _, u := range users {
			if len(fx.jobsOf(u)) > 0 {
				out = append(out, u)
			}
		}
		return out
	}
	if n, _ := fx.runProactive(s); n != 3 {
		t.Fatalf("first run started %d, want the limit 3", n)
	}
	if got := startedBy(); !slices.Equal(got, []string{users[0], users[2], users[3]}) {
		t.Fatalf("first run: %v", got)
	}
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(s); n != 3 {
		t.Fatalf("second run started %d, want 3", n)
	}
	if got := startedBy(); !slices.Equal(got, []string{users[0], users[2], users[3], users[4], users[5], users[6]}) {
		t.Fatalf("second run continues after the first: %v", got)
	}
	// the rotation wraps; the paused user is still skipped, nobody runs twice
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(s); n != 0 {
		t.Fatalf("third run started %d, want 0", n)
	}
	for _, u := range users {
		if got := len(fx.jobsOf(u)); got > 1 {
			t.Errorf("%s has %d jobs", u, got)
		}
	}
	// after the interval the rotation starts with whoever comes next, so a
	// user late in the order is not starved by the limit
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.ClearPause(users[1], store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		for _, j := range fx.jobsOf(u) {
			if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(u, j.ID) }); err != nil && !errors.Is(err, store.ErrConflict) {
				t.Fatal(err)
			}
		}
	}
	fx.clk.Advance(time.Hour)
	total := 0
	for range 3 {
		n, _ := fx.runProactive(s)
		total += n
		fx.clk.Advance(time.Minute)
	}
	if total != 7 {
		t.Fatalf("started %d over three runs, want all 7", total)
	}
	for _, j := range append(fx.jobsOf(users[1]), fx.jobsOf(users[6])...) {
		if j.Goal != reviewGoal || !store.IsProactiveGoal(j.Goal) {
			t.Errorf("goal %q is not the fixed template", j.Goal)
		}
	}
}

func TestProactiveTaskRunsEveryMinuteFromTick(t *testing.T) {
	fx := newFixture(t)
	u := fx.addUsers(1)[0]
	fx.enableProactive(u, time.Hour)
	s := fx.scheduler()
	res := s.Tick(bg)
	if !slices.Contains(res.Ran, TaskProactive) || res.Counts[TaskProactive] != 1 {
		t.Fatalf("tick = %+v", res)
	}
	fx.clk.Advance(59 * time.Second)
	if res := s.Tick(bg); slices.Contains(res.Ran, TaskProactive) {
		t.Fatal("ran again within the minute")
	}
	fx.clk.Advance(time.Second)
	if res := s.Tick(bg); !slices.Contains(res.Ran, TaskProactive) {
		t.Fatal("not run after a minute")
	}
	if strings.Contains(fx.logs.String(), reviewGoal) {
		t.Error("the log carries the goal")
	}
}

func TestProactiveConfigIsValidated(t *testing.T) {
	fx := newFixture(t)
	for _, c := range []Config{
		{Proactive: ProactiveConfig{PerTick: -1}},
		{Proactive: ProactiveConfig{Limits: store.ProactiveLimits{DailyJobs: -1}}},
		{Intervals: Intervals{Proactive: -time.Second}},
	} {
		if _, err := New(fx.deps, c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	s := fx.scheduler()
	if s.c.Proactive.PerTick != 50 || s.c.Proactive.Limits.DailyJobs != 24 || s.c.Intervals.Proactive != time.Minute {
		t.Errorf("defaults = %+v, %v", s.c.Proactive, s.c.Intervals.Proactive)
	}
}
