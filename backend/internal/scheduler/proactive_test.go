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

// cancelLive cancels every live job of the users.
func (fx *fixture) cancelLive(users ...string) {
	fx.t.Helper()
	for _, u := range users {
		for _, j := range fx.jobsOf(u) {
			if j.State.Terminal() {
				continue
			}
			if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(u, j.ID) }); err != nil {
				fx.t.Fatal(err)
			}
		}
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
		skip    string // the audited skip reason; "" without start: no job and no audit
		start   bool
	}{
		{"due", time.Hour, nil, "", true},
		{"long overdue", 30 * 24 * time.Hour, nil, "", true},
		{"interval not passed", time.Hour - time.Second, nil, "", false},
		{"disabled", time.Hour, func(fx *fixture, u string) {
			fx.enableProactive(u, time.Hour, func(p *store.Policy) { p.ProactiveEnabled = false })
		}, "", false},
		{"paused by the user", time.Hour, pausedFor(store.PauseUser), "", false},
		{"dormant", time.Hour, pausedFor(store.PauseDormant), "", false},
		{"token anomaly", time.Hour, pausedFor(store.PauseTokenAnomaly), "", false},
		{"credentials invalid", time.Hour, pausedFor(store.PauseCredentialInvalid), "", false},
		{"credentials revoked", time.Hour, pausedFor(store.PauseCredentialRevoked), "", false},
		{"kill switch in the database", time.Hour, func(fx *fixture, _ string) {
			if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) }); err != nil {
				t.Fatal(err)
			}
		}, "", false},
		{"kill switch in the environment", time.Hour, func(fx *fixture, _ string) { fx.cfg.Proactive.Limits.KillSwitchEnv = true }, "", false},
		{"quiet hours wrapping midnight", time.Hour, quiet("22:00-12:01"), "", false},
		{"quiet hours starting now", time.Hour, quiet("12:00-13:00"), "", false},
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

func TestProactiveSkipAuditing(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(3) // live job, dormant, quiet hours
	fx.enableProactive(users[0], time.Hour)
	fx.insertJob(users[0], store.TriggerManual, store.JobQueued)
	fx.enableProactive(users[1], time.Hour)
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(users[1], store.PauseDormant); return err }); err != nil {
		t.Fatal(err)
	}
	fx.enableProactive(users[2], time.Hour, func(p *store.Policy) { p.QuietHours = "00:00-23:59" })
	skips := func(u string) []string { return fx.auditOf(u, audit.KindProactiveSkipped) }

	s := fx.scheduler()
	for i := range 48 * 60 {
		if i%7 == 0 {
			s = fx.scheduler() // restarts keep nothing in memory
		}
		fx.runProactive(s)
		fx.clk.Advance(time.Minute)
	}
	if got := skips(users[1]); len(got) != 0 {
		t.Errorf("a dormant user got %d skip events in 48h", len(got))
	}
	for _, e := range skips(users[2]) {
		if strings.Contains(e, store.SkipQuietHours) {
			t.Errorf("quiet hours were audited: %s", e)
		}
	}
	// live_job: once, then once more after 24h, restarts included
	if got := skips(users[0]); len(got) != 2 || got[0] != "skipped trigger=schedule reason=live_job" {
		t.Fatalf("live_job skips over 48h = %v, want 2", got)
	}
	// a change of reason is recorded at once
	fx.cancelLive(users...) // useTokens claims the oldest queued job of anyone
	fx.useTokens(users[0], 1<<40)
	fx.cfg.Proactive.Limits.DailyTokens = 1000
	s = fx.scheduler()
	fx.runProactive(s)
	if got := skips(users[0]); len(got) != 3 || got[2] != "skipped trigger=schedule reason=budget" {
		t.Fatalf("after the budget ran out: %v", got)
	}
	if len(fx.jobsOf(users[0])) != 2 { // the cancelled manual one and the chat job
		t.Fatal("a job was started despite the skips")
	}
}

func TestProactiveSkippedUsersWaitBeforeTheNextCheck(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.Proactive.Limits = store.ProactiveLimits{DailyJobs: 2, DailyTokens: 1000}
	users := fx.addUsers(5)
	for _, u := range users {
		fx.enableProactive(u, time.Hour)
	}
	// u0 live job (2m), u1 paused (5m), u2 daily cap of 2 (midnight), u3 budget
	// (15m), u4 quiet hours 12:00-12:30 (until 12:30)
	fx.useTokens(users[3], 900) // first: it claims the oldest queued job
	fx.insertJob(users[0], store.TriggerManual, store.JobQueued)
	if err := fx.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(users[1], store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	fx.insertJob(users[2], store.TriggerManual, store.JobCancelled)
	fx.insertJob(users[2], store.TriggerManual, store.JobCancelled)
	fx.enableProactive(users[4], time.Hour, func(p *store.Policy) { p.QuietHours = "12:00-12:30" })
	spy := &viewSpy{Store: fx.st}
	fx.deps.Store = spy
	s := fx.scheduler()
	fx.runProactive(s)
	if got := s.trackedNotBefore(); got != 5 {
		t.Fatalf("tracked %d users, want 5", got)
	}
	want := map[string]time.Time{
		users[0]: fx.clk.Now().Add(2 * time.Minute),
		users[1]: fx.clk.Now().Add(5 * time.Minute),
		users[2]: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC),
		users[3]: fx.clk.Now().Add(15 * time.Minute),
		users[4]: time.Date(2026, 10, 16, 12, 30, 0, 0, time.UTC),
	}
	for u, at := range want {
		if got := s.notBeforeOf(u); !got.Equal(at) {
			t.Errorf("%s not before %v, want %v", u, got, at)
		}
	}
	// within the waits a run reads the due list only, no user
	fx.clk.Advance(time.Minute)
	spy.reset()
	fx.runProactive(s)
	if got := spy.views(); got != 2 { // the kill switch and one page
		t.Errorf("%d read transactions, want 2", got)
	}
	// the condition ended early: the user is picked up after the wait
	for _, j := range fx.jobsOf(users[0]) {
		if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(users[0], j.ID) }); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := fx.runProactive(s); n != 0 {
		t.Fatalf("started %d during the wait", n)
	}
	fx.clk.Advance(time.Minute)
	if n, _ := fx.runProactive(s); n != 1 || len(fx.jobsOf(users[0])) != 2 {
		t.Fatalf("started %d after the wait, want users[0]", n)
	}
	// expired waits are forgotten
	fx.clk.Advance(24 * time.Hour)
	for _, u := range users {
		if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.DeleteUser(u) }); err != nil {
			t.Fatal(err)
		}
	}
	fx.runProactive(s)
	if got := s.trackedNotBefore(); got != 0 {
		t.Errorf("tracked %d users after their waits passed", got)
	}
}

// viewSpy counts read transactions.
type viewSpy struct {
	store.Store
	mu sync.Mutex
	n  int
}

func (s *viewSpy) View(ctx context.Context, fn func(tx store.Tx) error) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.Store.View(ctx, fn)
}

func (s *viewSpy) views() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *viewSpy) reset() {
	s.mu.Lock()
	s.n = 0
	s.mu.Unlock()
}

func TestProactiveWakesOnlyAfterAStart(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(2)
	fx.enableProactive(users[0], -1) // baseline only
	fx.enableProactive(users[1], time.Hour)
	fx.insertJob(users[1], store.TriggerManual, store.JobQueued) // skip only
	s := fx.scheduler()
	if n, _ := fx.runProactive(s); n != 0 {
		t.Fatalf("started %d", n)
	}
	if fx.f.wakes() != 0 {
		t.Errorf("woken %d times without a start", fx.f.wakes())
	}
	if fx.lastSweep(users[0]) == nil || len(fx.auditOf(users[1], audit.KindProactiveSkipped)) != 1 {
		t.Fatal("the baseline or the skip is missing")
	}
}

func TestProactiveKillSwitchWritesNothing(t *testing.T) {
	for _, env := range []bool{false, true} {
		fx := newFixture(t)
		users := fx.addUsers(2)
		fx.enableProactive(users[0], -1)
		fx.enableProactive(users[1], time.Hour)
		fx.insertJob(users[1], store.TriggerManual, store.JobQueued)
		if env {
			fx.cfg.Proactive.Limits.KillSwitchEnv = true
		} else if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) }); err != nil {
			t.Fatal(err)
		}
		fx.runProactive(fx.scheduler())
		if fx.lastSweep(users[0]) != nil {
			t.Errorf("env=%v: a baseline was written under the kill switch", env)
		}
		if got := fx.auditOf(users[1], audit.KindProactiveSkipped); len(got) != 0 {
			t.Errorf("env=%v: skips = %v", env, got)
		}
	}
}

// The limit reached on the last user of a short page wraps the rotation at
// once: the next run starts from the first user, not with an empty page.
func TestProactiveLimitOnTheLastUserWraps(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.Batch = 5
	fx.cfg.Proactive.PerTick = 2
	users := fx.addUsers(4)
	fx.enableProactive(users[0], time.Hour)
	fx.enableProactive(users[3], time.Hour)
	s := fx.scheduler()
	if n, _ := fx.runProactive(s); n != 2 {
		t.Fatalf("started %d", n)
	}
	if s.pro.after != "" {
		t.Fatalf("cursor = %q, want wrapped", s.pro.after)
	}
	// users[1] becomes due; the next run finds it at once
	fx.enableProactive(users[1], time.Hour)
	if n, _ := fx.runProactive(s); n != 1 || len(fx.jobsOf(users[1])) != 1 {
		t.Fatalf("started %d, want users[1]", n)
	}
	// on a full page the cursor stays at the last user
	fx.cfg.Batch = 2
	fx.cfg.Proactive.PerTick = 1
	fx.enableProactive(users[2], time.Hour)
	fx.enableProactive(users[3], -1)
	fx.cancelLive(users...)
	s2 := fx.scheduler()
	fx.clk.Advance(2 * time.Hour)
	if n, _ := fx.runProactive(s2); n != 1 || s2.pro.after != users[0] {
		t.Fatalf("started %d, cursor %q", n, s2.pro.after)
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
		{Proactive: ProactiveConfig{Limits: store.ProactiveLimits{DailyTokens: -1}}},
		{}, // the daily token cap is required: 0 would mean no budget
	} {
		if _, err := New(fx.deps, c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	fx.cfg.Proactive = ProactiveConfig{Limits: store.ProactiveLimits{DailyTokens: 1000}}
	s := fx.scheduler()
	if s.c.Proactive.PerTick != 50 || s.c.Proactive.Limits.DailyJobs != 24 || s.c.Intervals.Proactive != time.Minute {
		t.Errorf("defaults = %+v, %v", s.c.Proactive, s.c.Intervals.Proactive)
	}
}
