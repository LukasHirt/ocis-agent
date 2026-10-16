package agent

import (
	"context"
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

// fakeCreds is a scripted Credentials: it hands out the fake's app token
// unless err is set, and counts HandleUnauthorized reports.
type fakeCreds struct {
	mu           sync.Mutex
	err          error
	unauthorized atomic.Int32
}

func (c *fakeCreds) AppCreds(context.Context, string) (ocisclient.Creds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return ocisclient.Creds{}, c.err
	}
	return ocisclient.AppToken(login, password), nil
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
		return errInjected
	}
	return nil
}

func (s *faultStore) InTx(ctx context.Context, fn func(store.Tx) error) error {
	return s.Store.InTx(ctx, func(tx store.Tx) error { return fn(faultTx{Tx: tx, s: s}) })
}

type faultTx struct {
	store.Tx
	s *faultStore
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

func (t faultTx) FinishIdempotent(userID, key string, state store.IdemState, result json.RawMessage) error {
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
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"), sqlite.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	faults := &faultStore{Store: db, calls: map[string]int{}, fails: map[string]int{}}
	var st store.Store = faults
	if err := st.InTx(bg, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: userID, Name: login, PersonalDriveID: drive})
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, faults: faults, clk: clk, fake: fake, client: client, creds: &fakeCreds{}}
	f.mem = memory.New(client, st, memory.Limits{})
	if err := f.mem.Ensure(bg, f.sess()); err != nil {
		t.Fatal(err)
	}
	if f.reg, err = tools.NewDefault(); err != nil {
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
	f.cfg = Config{Now: clk.Now}
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
