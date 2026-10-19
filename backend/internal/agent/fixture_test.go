package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
	"github.com/owncloud/ocis-agent/backend/internal/tools"

	_ "modernc.org/sqlite" // the driver for rawSQL
)

var bg = context.Background()

const (
	userID   = "u1"
	login    = "einstein"
	password = "pw"
	drive    = "drive1"

	modelDefault = "model-default"
	modelChat    = "model-chat"
	modelPro     = "model-pro"
)

type clock struct {
	mu     sync.Mutex
	t      time.Time
	follow time.Time // if set, the clock moves with real time since then
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.follow.IsZero() {
		return c.t.Add(time.Since(c.follow))
	}
	return c.t
}

// Follow makes the clock move with real time from now on.
func (c *clock) Follow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.follow = time.Now()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeCreds is a scripted Credentials: it hands out the fake's app token
// unless err is set, and counts HandleUnauthorized reports.
type fakeCreds struct {
	mu           sync.Mutex
	err          error
	secret       string // "" means the fake's password
	unauthorized atomic.Int32
}

func (c *fakeCreds) AppCreds(context.Context, string) (ocisclient.Creds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return ocisclient.Creds{}, c.err
	}
	if c.secret != "" {
		return ocisclient.AppToken(login, ocisclient.Secret(c.secret)), nil
	}
	return ocisclient.AppToken(login, password), nil
}

// setSecret makes AppCreds hand out another token, like a renewal.
func (c *fakeCreds) setSecret(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secret = s
}

func (c *fakeCreds) HandleUnauthorized(context.Context, string) error {
	c.unauthorized.Add(1)
	return nil
}

func (c *fakeCreds) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

var errInjected = errors.New("injected store fault")

// faultStore wraps a store and can fail the nth call (1-based, counted from
// arming) of selected Tx methods with errInjected.
type faultStore struct {
	store.Store
	mu    sync.Mutex
	calls map[string]int
	fails map[string]int
	errs  map[string]error // the error of a failing method (errInjected if unset)
	after map[string]func()
}

// afterCommit runs fn once, outside any transaction, right after the next
// committed transaction that called method (BeginIdempotent or
// FinishIdempotent): a hook between the executor's transactions A and B.
func (s *faultStore) afterCommit(method string, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.after[method] = fn
}

func (s *faultStore) failAt(method string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method], s.fails[method] = 0, n
}

func (s *faultStore) hit(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[method]++
	if n := s.fails[method]; n > 0 && s.calls[method] == n {
		if err := s.errs[method]; err != nil {
			return err
		}
		return errInjected
	}
	return nil
}

// failWith makes the nth call of method fail with err.
func (s *faultStore) failWith(method string, n int, err error) {
	s.failAt(method, n)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[method] = err
}

// View wraps the read-only transaction like InTx (no commit hooks).
func (s *faultStore) View(ctx context.Context, fn func(store.Tx) error) error {
	return s.Store.View(ctx, func(tx store.Tx) error {
		return fn(faultTx{Tx: tx, s: s, called: map[string]bool{}})
	})
}

func (s *faultStore) InTx(ctx context.Context, fn func(store.Tx) error) error {
	called := map[string]bool{}
	err := s.Store.InTx(ctx, func(tx store.Tx) error {
		clear(called)
		return fn(faultTx{Tx: tx, s: s, called: called})
	})
	if err != nil {
		return err
	}
	for m := range called {
		s.mu.Lock()
		hook := s.after[m]
		delete(s.after, m)
		s.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
	return nil
}

type faultTx struct {
	store.Tx
	s      *faultStore
	called map[string]bool
}

func (t faultTx) BeginIdempotent(rec store.IdemRecord) (*store.IdemRecord, error) {
	t.called["BeginIdempotent"] = true
	return t.Tx.BeginIdempotent(rec)
}

func (t faultTx) SaveCheckpoint(userID, jobID string, cp json.RawMessage, fence int64) error {
	if err := t.s.hit("SaveCheckpoint"); err != nil {
		return err
	}
	return t.Tx.SaveCheckpoint(userID, jobID, cp, fence)
}

func (t faultTx) InsertApproval(a store.Approval, fence int64) (store.Approval, error) {
	if err := t.s.hit("InsertApproval"); err != nil {
		return store.Approval{}, err
	}
	return t.Tx.InsertApproval(a, fence)
}

func (t faultTx) InsertMessage(m store.Message) (store.Message, error) {
	if err := t.s.hit("InsertMessage"); err != nil {
		return store.Message{}, err
	}
	return t.Tx.InsertMessage(m)
}

func (t faultTx) ListRecentMessages(userID, conv string, n int, excludeJobID string) ([]store.Message, error) {
	if err := t.s.hit("ListRecentMessages"); err != nil {
		return nil, err
	}
	return t.Tx.ListRecentMessages(userID, conv, n, excludeJobID)
}

func (t faultTx) JobStatuses(userID string, ids []string) (map[string]store.JobStatus, error) {
	if err := t.s.hit("JobStatuses"); err != nil {
		return nil, err
	}
	return t.Tx.JobStatuses(userID, ids)
}

func (t faultTx) FinishIdempotent(userID, key string, state store.IdemState, result json.RawMessage) error {
	t.called["FinishIdempotent"] = true
	if err := t.s.hit("FinishIdempotent"); err != nil {
		return err
	}
	return t.Tx.FinishIdempotent(userID, key, state, result)
}

type fixture struct {
	t        *testing.T
	st       store.Store
	faults   *faultStore
	clk      *clock
	fake     *fakeocis.Server
	client   *ocisclient.Client
	mem      *memory.Service
	reg      *tools.Registry
	gate     *approvals.Gate
	ex       *approvals.Executor
	q        *queue.Queue
	creds    *fakeCreds
	catalog  llm.Catalog
	cfg      Config
	provider llm.Provider
	lastJob  string
	dbPath   string
	calls    *toolCounter
	pastJob  string // the job of the last user message added by addMessage
}

// newFixture builds a runner's world: a SQLite store and the gate on the
// fixture clock, a fake oCIS with the enrolled user's personal drive (memory
// folders created), the default tools and a catalog of three models. The
// provider is set per test.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := newClock()
	fake := fakeocis.New(t)
	fake.AddUser(login, password, userID, drive)
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	db, err := sqlite.Open(dbPath, sqlite.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	faults := &faultStore{Store: db, calls: map[string]int{}, fails: map[string]int{}, errs: map[string]error{}, after: map[string]func(){}}
	var st store.Store = faults
	if err := st.InTx(bg, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: userID, Name: login, PersonalDriveID: drive})
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, faults: faults, clk: clk, fake: fake, client: client, creds: &fakeCreds{}, dbPath: dbPath}
	f.mem = memory.New(client, st, memory.Limits{})
	if err := f.mem.Ensure(bg, f.sess()); err != nil {
		t.Fatal(err)
	}
	f.calls = &toolCounter{n: map[string]int{}}
	if f.reg, err = countingRegistry(f.calls); err != nil {
		t.Fatal(err)
	}
	acfg := approvals.Config{Now: clk.Now}
	if f.gate, err = approvals.NewGate(st, f.reg, acfg); err != nil {
		t.Fatal(err)
	}
	if f.ex, err = approvals.NewExecutor(st, f.reg, acfg); err != nil {
		t.Fatal(err)
	}
	f.q = queue.New(st, queue.WithLeaseTTL(2*time.Minute))
	if f.catalog, err = llm.NewCatalog(modelDefault, []llm.ModelInfo{{ID: modelChat}, {ID: modelPro}}); err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{Now: clk.Now, UserDailyTokens: 1000000}
	return f
}

func (f *fixture) sess() memory.Session {
	return memory.Session{UserID: userID, DriveID: drive, Creds: ocisclient.AppToken(login, password)}
}

func (f *fixture) deps() Deps {
	return Deps{
		Store: f.st, Queue: f.q, Provider: f.provider, Catalog: f.catalog, Registry: f.reg,
		Gate: f.gate, Executor: f.ex,
		Env:   EnvBuilder{Creds: f.creds, Store: f.st, Files: f.client, Memory: f.mem},
		Creds: f.creds, Cfg: f.cfg,
	}
}

func (f *fixture) runner() *Runner {
	f.t.Helper()
	r, err := NewRunner(f.deps())
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// enqueue adds a chat job and returns its id.
func (f *fixture) enqueue(goal string) string { return f.enqueueKind(store.TriggerChat, goal) }

func (f *fixture) enqueueKind(kind, goal string) string {
	f.t.Helper()
	j, err := f.q.Enqueue(bg, store.Job{UserID: userID, TriggerKind: kind, Goal: goal})
	if err != nil {
		f.t.Fatal(err)
	}
	f.lastJob = j.ID
	return j.ID
}

// claim claims the next runnable job like a worker would.
func (f *fixture) claim() store.Claim {
	f.t.Helper()
	c, ok := f.tryClaim()
	if !ok {
		f.t.Fatal("nothing to claim")
	}
	return c
}

func (f *fixture) tryClaim() (store.Claim, bool) {
	f.t.Helper()
	var c store.Claim
	var ok bool
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		var err error
		c, ok, err = tx.ClaimNextJob("w1", 2*time.Minute)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return c, ok
}

// runOnce claims the next job and runs it; it fails the test on an error.
func (f *fixture) runOnce() {
	f.t.Helper()
	if err := f.runner().Run(bg, f.claim()); err != nil {
		f.t.Fatalf("Run: %v", err)
	}
}

func (f *fixture) job() store.Job {
	f.t.Helper()
	var j store.Job
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(userID, f.lastJob)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) checkpoint() Checkpoint {
	f.t.Helper()
	cp, err := ParseCheckpoint(f.job().Checkpoint)
	if err != nil {
		f.t.Fatal(err)
	}
	return cp
}

func (f *fixture) steps() []store.JobStep {
	f.t.Helper()
	var s []store.JobStep
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		s, err = tx.ListSteps(userID, f.lastJob)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) audit() []audit.Record {
	f.t.Helper()
	var r []audit.Record
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		r, err = tx.ReadAudit(userID, 0, 1000)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) auditKinds() string {
	var k []string
	for _, r := range f.audit() {
		k = append(k, r.Kind)
	}
	return strings.Join(k, ",")
}

func (f *fixture) auditOf(kind string) []audit.Record {
	var out []audit.Record
	for _, r := range f.audit() {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (f *fixture) approvals() []store.Approval {
	f.t.Helper()
	var out []store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		out, err = tx.ListApprovals(userID, store.ApprovalFilter{JobID: f.lastJob, Limit: 500})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) setPolicy(fn func(*store.Policy)) {
	f.t.Helper()
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		p, err := tx.GetPolicy(userID)
		if err != nil {
			return err
		}
		fn(&p)
		return tx.PutPolicy(p)
	}); err != nil {
		f.t.Fatal(err)
	}
}

// decide records the user's decision on an approval.
func (f *fixture) decide(a store.Approval, d store.ApprovalState) {
	f.t.Helper()
	if _, err := f.gate.Decide(bg, userID, "oidc-sub", []approvals.DecisionItem{{ApprovalID: a.ID, Decision: d, ArgsHash: a.ArgsHash}}); err != nil {
		f.t.Fatal(err)
	}
}

func tc(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: args}
}

func args(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// toolMessages returns the tool messages of a transcript.
func toolMessages(ms []llm.Message) []llm.Message {
	var out []llm.Message
	for _, m := range ms {
		if m.Role == llm.RoleTool {
			out = append(out, m)
		}
	}
	return out
}

// rawSQL changes exactly one row of the database behind the store's back, to
// force a state the store's API never produces (a tampered or inconsistent
// row).
func (f *fixture) rawSQL(query string, a ...any) {
	f.t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		f.t.Fatal(err)
	}
	res, err := db.Exec(query, a...)
	if err != nil {
		f.t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		f.t.Fatalf("rawSQL changed %d rows, want 1", n)
	}
}

// takeover lets the lease expire, reaps the job and claims it as another
// worker, like a second sidecar after the first one stalled.
func (f *fixture) takeover() store.Claim {
	f.t.Helper()
	f.clk.Advance(3 * time.Minute)
	var c store.Claim
	var ok bool
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.ReapExpired(100, func(int) time.Duration { return 0 }); err != nil {
			return err
		}
		var err error
		c, ok, err = tx.ClaimNextJob("w2", 2*time.Minute)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	if !ok || c.Job.ID != f.lastJob {
		f.t.Fatalf("takeover claimed %v %q", ok, c.Job.ID)
	}
	return c
}

// modifying counts the requests that could change the file at p in the
// personal drive (PUT, DELETE, MKCOL, MOVE, COPY) the fake oCIS received.
func (f *fixture) modifying(p string) int {
	n := 0
	for _, r := range f.fake.Requests() {
		if r.Path != "/remote.php/dav/spaces/"+drive+p {
			continue
		}
		switch r.Method {
		case "PUT", "DELETE", "MKCOL", "MOVE", "COPY":
			n++
		}
	}
	return n
}

// seedJob enqueues a chat job with a stored checkpoint, as if a run had
// saved it and died.
func (f *fixture) seedJob(cp Checkpoint) {
	f.t.Helper()
	raw, err := cp.Marshal()
	if err != nil {
		f.t.Fatal(err)
	}
	j, err := f.q.Enqueue(bg, store.Job{UserID: userID, TriggerKind: store.TriggerChat, Goal: "g", Checkpoint: raw})
	if err != nil {
		f.t.Fatal(err)
	}
	f.lastJob = j.ID
}

func (f *fixture) idem(key string) store.IdemRecord {
	f.t.Helper()
	r, err := f.idemMaybe(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

// toolCounter counts the Prepare, Execute and Reconcile calls of the
// registry's tools, per tool and per (tool, canonical arguments).
type toolCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *toolCounter) add(op, tool string, args json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[op+"\x00"+tool]++
	c.n[op+"\x00"+tool+"\x00"+string(args)]++
}

func (c *toolCounter) get(op, tool string, args ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := op + "\x00" + tool
	if len(args) > 0 {
		k += "\x00" + args[0]
	}
	return c.n[k]
}

// countingTool delegates to the real tool and counts. It deliberately has no
// Reconcile method: countingReconciler adds it only for tools that have one,
// so the executor's type assertion sees what the real tool offers.
type countingTool struct {
	tools.Tool
	c *toolCounter
}

func (t *countingTool) Prepare(ctx context.Context, env tools.Env, args json.RawMessage) (tools.Prepared, error) {
	t.c.add("prepare", t.Name(), args)
	return t.Tool.Prepare(ctx, env, args)
}

func (t *countingTool) Execute(ctx context.Context, env tools.Env, args json.RawMessage, info tools.ExecInfo) (tools.Result, error) {
	t.c.add("execute", t.Name(), args)
	return t.Tool.Execute(ctx, env, args, info)
}

type countingReconciler struct {
	*countingTool
	rc tools.Reconciler
}

func (t *countingReconciler) Reconcile(ctx context.Context, env tools.Env, args json.RawMessage, info tools.ExecInfo) (tools.Reconciled, error) {
	t.c.add("reconcile", t.Name(), args)
	return t.rc.Reconcile(ctx, env, args, info)
}

// countingRegistry is the default registry with every tool wrapped.
func countingRegistry(c *toolCounter) (*tools.Registry, error) {
	def, err := tools.NewDefault()
	if err != nil {
		return nil, err
	}
	var ts []tools.Tool
	for _, name := range def.Names() {
		inner, _ := def.Get(name)
		ct := &countingTool{Tool: inner, c: c}
		if rc, ok := inner.(tools.Reconciler); ok {
			ts = append(ts, &countingReconciler{countingTool: ct, rc: rc})
		} else {
			ts = append(ts, ct)
		}
	}
	return tools.NewRegistry(ts...)
}

// executes counts the Execute calls of a tool, optionally only those with
// these canonical arguments.
func (f *fixture) executes(tool string, args ...string) int {
	return f.calls.get("execute", tool, args...)
}

func (f *fixture) reconciles(tool string) int { return f.calls.get("reconcile", tool) }

// idemMaybe returns the idempotency record for key, or store.ErrNotFound.
func (f *fixture) idemMaybe(key string) (store.IdemRecord, error) {
	var r store.IdemRecord
	err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		r, err = tx.GetIdempotent(userID, key)
		return err
	})
	return r, err
}
