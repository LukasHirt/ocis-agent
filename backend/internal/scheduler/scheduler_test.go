package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// The real components satisfy the scheduler's interfaces.
var (
	_ Queue      = (*queue.Queue)(nil)
	_ Settler    = (*approvals.Gate)(nil)
	_ Reconciler = (*approvals.Executor)(nil)
	_ Auth       = (*auth.Service)(nil)
	_            = agent.EnvBuilder{}.EnvFunc
)

// The database-only tasks run first, the oCIS-bound ones (each under its own
// deadline) after them.
var allTasks = []string{TaskReap, TaskExpire, TaskSettle, TaskDormant, TaskProactive, TaskReconcile, TaskRenew, TaskSweep}

func TestTickRunsEveryTaskOnce(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(3)
	fx.cfg.Batch = 7
	s := fx.scheduler()

	res := s.Tick(bg)
	if !slices.Equal(res.Ran, allTasks) || len(res.Failed) != 0 {
		t.Fatalf("result = %+v, want every task once and no failure", res)
	}
	for _, task := range allTasks {
		if task == TaskProactive {
			continue // reads the store, not a fake
		}
		want := 1
		if task == TaskSweep {
			want = len(users)
		}
		if got := fx.f.count(task); got != want {
			t.Errorf("%s ran %d times, want %d", task, got, want)
		}
	}
	if got := fx.f.sweptUsers(); !slices.Equal(got, users) {
		t.Errorf("swept %v, want %v", got, users)
	}
	if fx.f.maxAtt != 3 {
		t.Errorf("Reap got maxAttempts %d, want 3", fx.f.maxAtt)
	}
	for _, task := range []string{TaskSettle, TaskReconcile, TaskRenew, TaskDormant} {
		if got := fx.f.limits[task]; got != 7 {
			t.Errorf("%s got limit %d, want the batch 7", task, got)
		}
	}
	if res.Counts[TaskReap] != 2 || res.Counts[TaskExpire] != 1 || res.Counts[TaskSweep] != 3 {
		t.Errorf("counts = %v", res.Counts)
	}

	// At the same instant nothing is due again.
	if again := s.Tick(bg); len(again.Ran) != 0 {
		t.Fatalf("second tick ran %v", again.Ran)
	}
}

func TestTheBatchDefaultsTo100(t *testing.T) {
	fx := newFixture(t)
	fx.scheduler().Tick(bg)
	if got := fx.f.limits[TaskSettle]; got != 100 {
		t.Fatalf("limit = %d, want 100", got)
	}
}

func TestTasksRunAtTheirOwnIntervals(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.LeaseTTL = 40 * time.Second // reap every 20s
	s := fx.scheduler()
	s.Tick(bg)

	step := func(d time.Duration) []string {
		fx.clk.Advance(d)
		return s.Tick(bg).Ran
	}
	if ran := step(19 * time.Second); slices.Contains(ran, TaskReap) {
		t.Fatalf("reap ran after 19s: %v", ran)
	}
	if ran := step(time.Second); !slices.Contains(ran, TaskReap) || slices.Contains(ran, TaskExpire) {
		t.Fatalf("at 20s ran %v, want reap but not expire", ran)
	}
	if ran := step(39 * time.Second); slices.Contains(ran, TaskExpire) {
		t.Fatalf("expire ran after 59s: %v", ran)
	}
	ran := step(time.Second)
	for _, task := range []string{TaskExpire, TaskSettle, TaskReconcile, TaskRenew} {
		if !slices.Contains(ran, task) {
			t.Fatalf("at 1m ran %v, want %s", ran, task)
		}
	}
	if slices.Contains(ran, TaskDormant) {
		t.Fatalf("dormant ran after 1m: %v", ran)
	}
	for i := 0; i < 58; i++ {
		if ran := step(time.Minute); slices.Contains(ran, TaskDormant) {
			t.Fatalf("dormant ran after %dm", i+2)
		}
	}
	if ran := step(time.Minute); !slices.Contains(ran, TaskDormant) {
		t.Fatalf("at 1h ran %v, want dormant", ran)
	}
	if got := fx.f.count(TaskDormant); got != 2 {
		t.Fatalf("dormant ran %d times, want 2", got)
	}
	if got, want := fx.f.count(TaskExpire), 61; got != want {
		t.Fatalf("expire ran %d times, want %d", got, want)
	}
}

func TestTheReapIntervalIsAtLeastOneSecond(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.LeaseTTL = time.Second
	s := fx.scheduler()
	s.Tick(bg)
	fx.clk.Advance(500 * time.Millisecond)
	if ran := s.Tick(bg).Ran; slices.Contains(ran, TaskReap) {
		t.Fatalf("reap ran after 500ms: %v", ran)
	}
	fx.clk.Advance(500 * time.Millisecond)
	if ran := s.Tick(bg).Ran; !slices.Contains(ran, TaskReap) {
		t.Fatalf("reap did not run after 1s: %v", ran)
	}
}

func TestAFailingOrPanickingTaskDoesNotStopTheOthers(t *testing.T) {
	fx := newFixture(t)
	fx.addUsers(2)
	fx.f.panics[TaskReap] = "boom"
	fx.f.fail[TaskExpire] = fmt.Errorf("wrapped: %w", ocisclient.ErrUnauthorized)
	fx.f.fail[TaskSettle] = fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
	fx.f.fail[TaskRenew] = errors.New("disk on fire")
	fx.f.panics[TaskSweep] = errors.New("sweep panic")
	s := fx.scheduler()

	res := s.Tick(bg)
	if !slices.Equal(res.Ran, allTasks) {
		t.Fatalf("ran %v, want every task", res.Ran)
	}
	want := map[string]string{TaskReap: "panic", TaskExpire: "unauthorized", TaskSettle: "unavailable", TaskRenew: "internal", TaskSweep: "panic"}
	for task, cat := range want {
		if res.Failed[task] != cat {
			t.Errorf("Failed[%s] = %q, want %q", task, res.Failed[task], cat)
		}
	}
	if len(res.Failed) != len(want) {
		t.Errorf("Failed = %v", res.Failed)
	}
	if fx.f.count(TaskReconcile) != 1 || fx.f.count(TaskDormant) != 1 {
		t.Fatalf("calls = %v", fx.f.counts())
	}

	// The next tick runs them all again (a failure does not disable a task).
	fx.clk.Advance(time.Hour)
	if res := s.Tick(bg); !slices.Equal(res.Ran, allTasks) {
		t.Fatalf("next tick ran %v", res.Ran)
	}
	if fx.f.count(TaskReap) != 2 || fx.f.count(TaskExpire) != 2 || fx.f.count(TaskDormant) != 2 {
		t.Fatalf("calls = %v", fx.f.counts())
	}
}

func TestPerItemErrorsAreCountedNotFatal(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(4)
	fx.f.sweepErr[users[3]] = fmt.Errorf("not enrolled: %w", auth.ErrCredentialInvalid) // skipped, not an error
	fx.f.recon = approvals.ReconcileSummary{Resolved: 2, Errors: []error{fmt.Errorf("x: %w", ocisclient.ErrUnauthorized), errors.New("y")}}
	fx.f.renew = []auth.RenewOutcome{{UserID: users[0]}, {UserID: users[1], Err: auth.ErrCredentialInvalid}}
	fx.f.sweepErr[users[2]] = errors.New("listing failed")
	res := fx.scheduler().Tick(bg)
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want none (item errors are not task failures)", res.Failed)
	}
	if res.Counts[TaskReconcile] != 2 || res.Errors[TaskReconcile] != 2 {
		t.Errorf("reconcile counts = %d / %d", res.Counts[TaskReconcile], res.Errors[TaskReconcile])
	}
	if res.Counts[TaskRenew] != 1 || res.Errors[TaskRenew] != 1 {
		t.Errorf("renew counts = %d / %d", res.Counts[TaskRenew], res.Errors[TaskRenew])
	}
	if res.Counts[TaskSweep] != 2 || res.Errors[TaskSweep] != 1 {
		t.Errorf("sweep counts = %d / %d", res.Counts[TaskSweep], res.Errors[TaskSweep])
	}
	logs := fx.logs.String()
	for _, want := range []string{"unauthorized=1", "internal=1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

func TestReconcileUsesTheClampedCutoff(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.CallTimeout = 3 * time.Minute
	fx.cfg.RetrySettle = 2 * time.Minute
	s := fx.scheduler()
	s.Tick(bg)
	if want := fx.clk.Now().Add(-5 * time.Minute); !fx.f.olderThan.Equal(want) {
		t.Fatalf("olderThan = %v, want %v", fx.f.olderThan, want)
	}
	fx.clk.Advance(time.Minute)
	s.Tick(bg)
	if want := fx.clk.Now().Add(-5 * time.Minute); !fx.f.olderThan.Equal(want) {
		t.Fatalf("olderThan = %v, want %v (read from the clock)", fx.f.olderThan, want)
	}
}

func TestSweepVisitsEachUserOncePerIntervalAndWraps(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(250)
	fx.cfg.Batch = 100
	fx.cfg.SweepInterval = 15 * time.Minute
	fx.cfg.Intervals.Sweep = 10 * time.Second
	s := fx.scheduler()

	// Three pages cover everyone once, in id order.
	for i := 0; i < 3; i++ {
		s.Tick(bg)
		fx.clk.Advance(10 * time.Second)
	}
	if got := fx.f.sweptUsers(); !slices.Equal(got, users) {
		t.Fatalf("first pass swept %d users, want all 250 in order", len(got))
	}
	// Until the interval is over, the wrapped cursor finds nobody due.
	for fx.clk.Now().Before(newClock().Now().Add(15 * time.Minute)) {
		s.Tick(bg)
		fx.clk.Advance(10 * time.Second)
	}
	if got := len(fx.f.sweptUsers()); got != 250 {
		t.Fatalf("swept %d times within the interval, want 250", got)
	}
	// After it, the next pass sweeps everyone exactly once more.
	for i := 0; i < 3; i++ {
		s.Tick(bg)
		fx.clk.Advance(10 * time.Second)
	}
	got := fx.f.sweptUsers()
	if len(got) != 500 || !slices.Equal(got[250:], users) {
		t.Fatalf("second pass swept %d users", len(got)-250)
	}
}

func TestTheSweepForgetsUsersThatAreGone(t *testing.T) {
	fx := newFixture(t)
	fx.addUsers(3)
	fx.cfg.Intervals.Sweep = 10 * time.Second
	s := fx.scheduler()
	s.Tick(bg) // sweeps all, cursor at the end
	fx.clk.Advance(10 * time.Second)
	s.Tick(bg) // wraps: nobody due
	if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.DeleteUser("u0001") }); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(10 * time.Second)
	s.Tick(bg)
	fx.clk.Advance(10 * time.Second)
	s.Tick(bg)
	if n := s.trackedSweeps(); n != 2 {
		t.Fatalf("tracking %d users, want 2", n)
	}
}

func TestTasksRunWhileKilledOrPaused(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(3)
	if err := agent.SetKillSwitch(bg, fx.st, true); err != nil {
		t.Fatal(err)
	}
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.PausePolicy(users[0], store.PauseUser); err != nil {
			return err
		}
		_, err := tx.PausePolicy(users[1], store.PauseTokenAnomaly)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := fx.scheduler().Tick(bg)
	if !slices.Equal(res.Ran, allTasks) || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := fx.f.sweptUsers(); !slices.Equal(got, users) {
		t.Fatalf("swept %v, want every user", got)
	}
}

func TestACancelledTickReturnsPromptly(t *testing.T) {
	fx := newFixture(t)
	fx.addUsers(5)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	fx.f.onCall = func(task string) {
		if task == TaskReconcile {
			cancel()
		}
	}
	res := fx.scheduler().Tick(ctx)
	if fx.f.count(TaskRenew) != 0 || fx.f.count(TaskSweep) != 0 || slices.Contains(res.Ran, TaskSweep) {
		t.Fatalf("a task ran after the cancel: %v", res.Ran)
	}
}

func TestACancelDuringASweepStopsAndResumesAtThatUser(t *testing.T) {
	for name, at := range map[string]int{"mid-page": 2, "last user of the page": 4} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t)
			users := fx.addUsers(10)
			fx.cfg.Batch = 5
			s := fx.scheduler()
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			calls := 0
			// the first `at` calls return at once; the next one hangs until
			// the context is cancelled
			fx.f.onCall = func(task string) {
				if task != TaskSweep {
					return
				}
				if calls++; calls == at+1 {
					fx.f.mu.Lock()
					fx.f.hung["sweep"] = true
					fx.f.mu.Unlock()
					go cancel()
				}
			}
			s.Tick(ctx)
			if got := fx.f.sweptUsers(); !slices.Equal(got, users[:at+1]) {
				t.Fatalf("first tick swept %v, want %v", got, users[:at+1])
			}
			fx.f.mu.Lock()
			fx.f.hung["sweep"] = false
			fx.f.onCall = nil
			fx.f.mu.Unlock()
			fx.clk.Advance(10 * time.Second)
			s.Tick(bg)
			// the interrupted user is swept again first: the cursor did not
			// pass it and its sweep time was not recorded
			got := fx.f.sweptUsers()[at+1:]
			if len(got) == 0 || got[0] != users[at] {
				t.Fatalf("next tick swept %v, want it to start at %s", got, users[at])
			}
		})
	}
}

func TestAHungOCISCannotWedgeTheTick(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(3)
	fx.cfg.TaskTimeout = 50 * time.Millisecond
	fx.f.renew = []auth.RenewOutcome{{UserID: users[0]}, {UserID: users[1]}, {UserID: users[2]}}
	fx.f.hung["reconcile"], fx.f.hung["renew.user"], fx.f.hung["sweep"] = true, true, true
	s := fx.scheduler()

	start := time.Now()
	res := s.Tick(bg)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Tick took %v with three 50ms deadlines", took)
	}
	if !slices.Equal(res.Ran, allTasks) {
		t.Fatalf("ran %v", res.Ran)
	}
	for _, task := range []string{TaskReap, TaskExpire, TaskSettle, TaskDormant} {
		if fx.f.count(task) != 1 || res.Failed[task] != "" {
			t.Fatalf("%s: calls %d, failed %q", task, fx.f.count(task), res.Failed[task])
		}
	}
	if res.Failed[TaskReconcile] != CategoryUnavailable {
		t.Fatalf("reconcile failed %q, want unavailable", res.Failed[TaskReconcile])
	}
	fx.f.mu.Lock()
	renewed := len(fx.f.renewed)
	fx.f.mu.Unlock()
	if renewed != 1 || len(fx.f.sweptUsers()) != 1 {
		t.Fatalf("renew calls %d, sweep calls %d: the batch must stop at the first unavailable item", renewed, len(fx.f.sweptUsers()))
	}
	if res.Errors[TaskRenew] != 1 || res.Errors[TaskSweep] != 1 {
		t.Fatalf("errors = %v", res.Errors)
	}
}

func TestTheBatchStopsAtTheFirstUnavailableItem(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(4)
	down := &ocisclient.StatusError{Method: "GET", Path: "/graph/v1.0/me", Status: 503}
	fx.f.renew = []auth.RenewOutcome{{UserID: users[0]}, {UserID: users[1], Err: down}, {UserID: users[2]}}
	fx.f.sweepErr[users[1]] = down
	s := fx.scheduler()
	res := s.Tick(bg)
	fx.f.mu.Lock()
	renewed := append([]string(nil), fx.f.renewed...)
	fx.f.mu.Unlock()
	if !slices.Equal(renewed, users[:2]) || !slices.Equal(fx.f.sweptUsers(), users[:2]) {
		t.Fatalf("renewed %v, swept %v: want both to stop after %s", renewed, fx.f.sweptUsers(), users[1])
	}
	if res.Counts[TaskRenew] != 1 || res.Errors[TaskRenew] != 1 || res.Counts[TaskSweep] != 1 || res.Errors[TaskSweep] != 1 {
		t.Fatalf("counts %v errors %v", res.Counts, res.Errors)
	}
	// The next sweep run resumes at the user that hit the outage.
	delete(fx.f.sweepErr, users[1])
	fx.clk.Advance(10 * time.Second)
	s.Tick(bg)
	if got := fx.f.sweptUsers()[2:]; !slices.Equal(got, users[1:]) {
		t.Fatalf("next run swept %v, want %v", got, users[1:])
	}
}

func TestAUserWhoseSweepFailsIsNotRetriedWithinTheInterval(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(2)
	fx.f.sweepErr[users[0]] = errors.New("listing failed")
	fx.cfg.Intervals.Sweep = 10 * time.Second
	s := fx.scheduler()
	for i := 0; i < 10; i++ {
		s.Tick(bg)
		fx.clk.Advance(10 * time.Second)
	}
	n := 0
	for _, u := range fx.f.sweptUsers() {
		if u == users[0] {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the failing user was swept %d times within the interval, want 1", n)
	}
}

func TestASweepRunMakesUpToBatchCalls(t *testing.T) {
	fx := newFixture(t)
	fx.addUsers(25)
	fx.cfg.Batch = 10
	fx.cfg.Intervals.Sweep = 10 * time.Second
	s := fx.scheduler()
	for i := 0; i < 3; i++ { // 0-9, 10-19, 20-24 and wrap
		s.Tick(bg)
		fx.clk.Advance(10 * time.Second)
	}
	var added []string
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		for i := 25; i < 37; i++ {
			id := fmt.Sprintf("u%04d", i)
			added = append(added, id)
			if err := tx.PutUser(store.User{ID: id, Name: "n" + id, PersonalDriveID: "d" + id}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// One run pages past the 25 users that are not due and stops after Batch
	// calls.
	s.Tick(bg)
	if got := fx.f.sweptUsers()[25:]; !slices.Equal(got, added[:10]) {
		t.Fatalf("the run swept %v, want %v", got, added[:10])
	}
	fx.clk.Advance(10 * time.Second)
	s.Tick(bg)
	if got := fx.f.sweptUsers()[35:]; !slices.Equal(got, added[10:]) {
		t.Fatalf("the next run swept %v, want %v", got, added[10:])
	}
}

func TestATokenAnomalyIsLoggedWithTheCountOnly(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(1)
	fx.f.sweepRes = auth.SweepResult{Unknown: 2}
	fx.scheduler().Tick(bg)
	logs := fx.logs.String()
	if !strings.Contains(logs, `msg="scheduler: token anomaly"`) || !strings.Contains(logs, "user="+users[0]) || !strings.Contains(logs, "unknown=2") {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	fx := newFixture(t)
	fx.cfg.Intervals.Tick = time.Millisecond
	s := fx.scheduler()
	before := runtime.NumGoroutine() // after the store has started its own
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for fx.f.count(TaskReap) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fx.f.count(TaskReap) != 1 {
		t.Fatalf("reap ran %d times, want 1 (the fake clock does not move)", fx.f.count(TaskReap))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}
	for i := 0; i < 100 && runtime.NumGoroutine() > before; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: %d before, %d after", before, n)
	}
}

func TestLogsContainOnlyCategories(t *testing.T) {
	fx := newFixture(t)
	users := fx.addUsers(2)
	secret := func(what string) error { return errors.New("SECRET token=abc " + what) }
	fx.f.panics[TaskReap] = "SECRET panic value"
	fx.f.fail[TaskExpire] = secret("expire")
	fx.f.fail[TaskSettle] = fmt.Errorf("SECRET %w", ocisclient.ErrUnauthorized)
	fx.f.fail[TaskDormant] = secret("dormant")
	fx.f.recon = approvals.ReconcileSummary{Errors: []error{secret("recon"), fmt.Errorf("SECRET %w", ocisclient.ErrUnauthorized)}}
	fx.f.renew = []auth.RenewOutcome{{UserID: users[0], Err: secret("renew")}}
	fx.f.sweepErr[users[1]] = secret("sweep")
	s := fx.scheduler()
	s.Tick(bg)

	fx.clk.Advance(time.Hour)
	fx.f.fail[TaskRenew] = secret("renew task")
	delete(fx.f.panics, TaskReap)
	fx.f.fail[TaskReap] = secret("reap")
	s.Tick(bg)

	logs := fx.logs.String()
	if strings.Contains(logs, "SECRET") || strings.Contains(logs, "token=") {
		t.Fatalf("logs leak error text:\n%s", logs)
	}
	for _, want := range []string{"category=panic", "category=internal", "category=unauthorized", "task=renew", "task=sweep", "task=reconcile"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

func TestNewValidates(t *testing.T) {
	fx := newFixture(t)
	if _, err := New(fx.deps, fx.cfg); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for name, mutate := range map[string]func(*Deps, *Config){
		"no store":              func(d *Deps, _ *Config) { d.Store = nil },
		"no queue":              func(d *Deps, _ *Config) { d.Queue = nil },
		"no gate":               func(d *Deps, _ *Config) { d.Gate = nil },
		"no executor":           func(d *Deps, _ *Config) { d.Executor = nil },
		"no env":                func(d *Deps, _ *Config) { d.Env = nil },
		"no auth":               func(d *Deps, _ *Config) { d.Auth = nil },
		"no log":                func(d *Deps, _ *Config) { d.Log = nil },
		"no clock":              func(d *Deps, _ *Config) { d.Now = nil },
		"negative ttl":          func(_ *Deps, c *Config) { c.LeaseTTL = -time.Second },
		"negative timeout":      func(_ *Deps, c *Config) { c.CallTimeout = -time.Second },
		"negative batch":        func(_ *Deps, c *Config) { c.Batch = -1 },
		"huge batch":            func(_ *Deps, c *Config) { c.Batch = 1001 },
		"negative attempt":      func(_ *Deps, c *Config) { c.MaxAttempts = -1 },
		"negative sweep":        func(_ *Deps, c *Config) { c.SweepInterval = -time.Minute },
		"negative tick":         func(_ *Deps, c *Config) { c.Intervals.Tick = -time.Second },
		"negative task timeout": func(_ *Deps, c *Config) { c.TaskTimeout = -time.Second },
	} {
		d, c := fx.deps, fx.cfg
		mutate(&d, &c)
		if _, err := New(d, c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// realFixture wires the scheduler to the real queue, gate and executor over
// the fixture store; only auth is faked.
func realFixture(t *testing.T) (*fixture, *Scheduler) {
	t.Helper()
	fx := newFixture(t)
	reg, err := tools.NewRegistry(tools.All()...)
	if err != nil {
		t.Fatal(err)
	}
	acfg := approvals.Config{Now: fx.clk.Now}
	gate, err := approvals.NewGate(fx.st, reg, acfg)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := approvals.NewExecutor(fx.st, reg, acfg)
	if err != nil {
		t.Fatal(err)
	}
	fx.deps.Queue = queue.New(fx.st, queue.WithLeaseTTL(time.Minute))
	fx.deps.Gate = gate
	fx.deps.Executor = ex
	fx.cfg.LeaseTTL = time.Minute
	return fx, fx.scheduler()
}

func TestSettleEndedIsWiredToTheRealGate(t *testing.T) {
	fx, s := realFixture(t)
	users := fx.addUsers(1)
	job, a := fx.waitingJob(users[0], time.Hour)
	if err := fx.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(users[0], job.ID) }); err != nil {
		t.Fatal(err)
	}
	res := s.Tick(bg)
	if res.Counts[TaskSettle] != 1 || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := fx.approval(users[0], a.ID).State; got != store.ApprovalExpired {
		t.Fatalf("approval = %s, want expired", got)
	}
}

func TestExpireAndReapAreWiredToTheRealStore(t *testing.T) {
	fx, s := realFixture(t)
	users := fx.addUsers(2)
	waiting, a := fx.waitingJob(users[0], time.Minute)
	var leased store.Job
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.InsertJob(store.Job{UserID: users[1], TriggerKind: "chat", Goal: "g"}); err != nil {
			return err
		}
		c, ok, err := tx.ClaimNextJob("w", time.Minute)
		if err != nil || !ok {
			return fmt.Errorf("claim: %v %v", ok, err)
		}
		leased = c.Job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fx.clk.Advance(2 * time.Minute)
	res := s.Tick(bg)
	if len(res.Failed) != 0 || res.Counts[TaskExpire] != 1 || res.Counts[TaskReap] != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := fx.approval(users[0], a.ID).State; got != store.ApprovalExpired {
		t.Fatalf("approval = %s, want expired", got)
	}
	if got := fx.job(users[0], waiting.ID).State; got != store.JobQueued {
		t.Fatalf("waiting job = %s, want queued", got)
	}
	if got := fx.job(users[1], leased.ID).State; got != store.JobQueued {
		t.Fatalf("leased job = %s, want requeued", got)
	}
}
