package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

var bg = context.Background()

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakes implements Reaper, Settler, Reconciler and Auth. It counts calls per
// task and can make a task fail (fail) or panic (panics).
type fakes struct {
	mu        sync.Mutex
	calls     map[string]int
	fail      map[string]error
	panics    map[string]any
	swept     []string
	sweepErr  map[string]error
	olderThan time.Time
	limits    map[string]int
	maxAtt    int
	recon     approvals.ReconcileSummary
	renew     []auth.RenewOutcome
	sweepRes  auth.SweepResult
	onCall    func(task string)
}

func newFakes() *fakes {
	return &fakes{calls: map[string]int{}, fail: map[string]error{}, panics: map[string]any{}, sweepErr: map[string]error{}, limits: map[string]int{}}
}

func (f *fakes) enter(task string, limit int) error {
	f.mu.Lock()
	f.calls[task]++
	f.limits[task] = limit
	err, hook := f.fail[task], f.onCall
	p, hasPanic := f.panics[task]
	f.mu.Unlock()
	if hook != nil {
		hook(task)
	}
	if hasPanic {
		panic(p)
	}
	return err
}

func (f *fakes) count(task string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[task]
}

func (f *fakes) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.calls {
		out[k] = v
	}
	return out
}

func (f *fakes) sweptUsers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.swept...)
}

func (f *fakes) Reap(_ context.Context, maxAttempts int) (store.ReapResult, error) {
	f.mu.Lock()
	f.maxAtt = maxAttempts
	f.mu.Unlock()
	if err := f.enter("reap", 0); err != nil {
		return store.ReapResult{}, err
	}
	return store.ReapResult{Requeued: 1, Failed: 1}, nil
}

func (f *fakes) ExpireAndResume(context.Context) (int, error) {
	if err := f.enter("expire", 0); err != nil {
		return 0, err
	}
	return 1, nil
}

func (f *fakes) SettleEnded(_ context.Context, limit int) (int, error) {
	if err := f.enter("settle", limit); err != nil {
		return 0, err
	}
	return 1, nil
}

func (f *fakes) ReconcileOrphans(_ context.Context, _ approvals.EnvFunc, olderThan time.Time, limit int) (approvals.ReconcileSummary, error) {
	f.mu.Lock()
	f.olderThan = olderThan
	sum := f.recon
	f.mu.Unlock()
	if err := f.enter("reconcile", limit); err != nil {
		return approvals.ReconcileSummary{}, err
	}
	return sum, nil
}

func (f *fakes) RenewDue(_ context.Context, limit int) ([]auth.RenewOutcome, error) {
	f.mu.Lock()
	out := f.renew
	f.mu.Unlock()
	if err := f.enter("renew", limit); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakes) Sweep(_ context.Context, userID string) (auth.SweepResult, error) {
	f.mu.Lock()
	f.swept = append(f.swept, userID)
	err, res := f.sweepErr[userID], f.sweepRes
	f.mu.Unlock()
	if ferr := f.enter("sweep", 0); ferr != nil {
		return auth.SweepResult{}, ferr
	}
	return res, err
}

func (f *fakes) PauseDormant(_ context.Context, limit int) (int, error) {
	if err := f.enter("dormant", limit); err != nil {
		return 0, err
	}
	return 1, nil
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	t    *testing.T
	clk  *clock
	st   store.Store
	f    *fakes
	logs *syncBuffer
	cfg  Config
	deps Deps
}

func noEnv(context.Context, string) (tools.Env, error) {
	return tools.Env{}, fmt.Errorf("no env in tests")
}

// newFixture builds a SQLite store on the fixture clock, fakes for every
// dependency and a debug-level logger writing into logs.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := newClock()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"), sqlite.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := newFakes()
	logs := &syncBuffer{}
	fx := &fixture{t: t, clk: clk, st: st, f: f, logs: logs}
	fx.deps = Deps{
		Store: st, Queue: f, Gate: f, Executor: f, Env: noEnv, Auth: f,
		Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: clk.Now,
	}
	fx.cfg = Config{MaxAttempts: 3, LeaseTTL: 2 * time.Minute, RetrySettle: 2 * time.Minute, CallTimeout: 5 * time.Minute, SweepInterval: 15 * time.Minute}
	return fx
}

func (fx *fixture) scheduler() *Scheduler {
	fx.t.Helper()
	s, err := New(fx.deps, fx.cfg)
	if err != nil {
		fx.t.Fatal(err)
	}
	return s
}

// addUsers adds n users with ids u0000, u0001, ... and returns the ids.
func (fx *fixture) addUsers(n int) []string {
	fx.t.Helper()
	ids := make([]string, n)
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		for i := range ids {
			ids[i] = fmt.Sprintf("u%04d", i)
			if err := tx.PutUser(store.User{ID: ids[i], Name: "name-" + ids[i], PersonalDriveID: "drive-" + ids[i]}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fx.t.Fatal(err)
	}
	return ids
}

// waitingJob inserts a job for user, claims it and releases it into
// waiting_approval with one pending approval that expires after ttl.
func (fx *fixture) waitingJob(user string, ttl time.Duration) (store.Job, store.Approval) {
	fx.t.Helper()
	var job store.Job
	var a store.Approval
	if err := fx.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.InsertJob(store.Job{UserID: user, TriggerKind: "chat", Goal: "g"}); err != nil {
			return err
		}
		c, ok, err := tx.ClaimNextJob("w", time.Minute)
		if err != nil || !ok {
			return fmt.Errorf("claim: %v %v", ok, err)
		}
		job = c.Job
		a, err = tx.InsertApproval(store.Approval{
			UserID: user, JobID: c.Job.ID, CallID: "c1", Tool: "files.delete", Risk: store.RiskDestructive,
			Args: json.RawMessage(`{"path":"/old.txt"}`), ArgsHash: "sha256:abc", TargetETag: `"e1"`,
			Rationale: "tidy", Preview: json.RawMessage(`{}`), IdemKey: c.Job.ID + ":c1:abc",
			ExpiresAt: fx.clk.Now().Add(ttl),
		}, c.Fence)
		if err != nil {
			return err
		}
		_, err = tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	return job, a
}

func (fx *fixture) job(user, id string) store.Job {
	fx.t.Helper()
	var j store.Job
	if err := fx.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(user, id)
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	return j
}

func (fx *fixture) approval(user, id string) store.Approval {
	fx.t.Helper()
	var a store.Approval
	if err := fx.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(user, id)
		return err
	}); err != nil {
		fx.t.Fatal(err)
	}
	return a
}
