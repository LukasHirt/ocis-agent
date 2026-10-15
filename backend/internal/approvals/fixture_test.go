package approvals

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

var bg = context.Background()

const (
	userID = "u1"
	drive  = "drive1"
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

// probe is a scripted tool: its behaviour is set per test, and it counts how
// often Execute really ran.
type probe struct {
	name  string
	risk  tools.Risk
	auto  bool
	prep  func() (tools.Prepared, error)
	run   func(tools.ExecInfo) (tools.Result, error)
	recon func(tools.ExecInfo) (tools.Reconciled, error)
	execs atomic.Int32

	mu         sync.Mutex
	last       tools.ExecInfo
	prepared   []json.RawMessage // arguments Prepare received
	riskParams []json.RawMessage // arguments Risk received
}

// lastInfo returns the ExecInfo of the latest Execute.
func (p *probe) lastInfo() tools.ExecInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// seen returns copies of the arguments Prepare and Risk received.
func (p *probe) seen() (prepare, risk []json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]json.RawMessage(nil), p.prepared...), append([]json.RawMessage(nil), p.riskParams...)
}

func (p *probe) Name() string            { return p.name }
func (p *probe) Description() string     { return "probe " + p.name }
func (p *probe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *probe) AutoAllowed() bool       { return p.auto }

func (p *probe) Risk(args json.RawMessage) tools.Risk {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.riskParams = append(p.riskParams, append(json.RawMessage(nil), args...))
	return p.risk
}

func (p *probe) Prepare(_ context.Context, _ tools.Env, args json.RawMessage) (tools.Prepared, error) {
	p.mu.Lock()
	p.prepared = append(p.prepared, append(json.RawMessage(nil), args...))
	p.mu.Unlock()
	if p.prep != nil {
		return p.prep()
	}
	return tools.Prepared{Preview: json.RawMessage(`{"action":"probe"}`), TargetETag: "\"t1\""}, nil
}

func (p *probe) Execute(_ context.Context, _ tools.Env, _ json.RawMessage, info tools.ExecInfo) (tools.Result, error) {
	p.execs.Add(1)
	p.mu.Lock()
	p.last = info
	p.mu.Unlock()
	if p.run != nil {
		return p.run(info)
	}
	return tools.Result{Summary: "probe ran"}, nil
}

func (p *probe) Reconcile(_ context.Context, _ tools.Env, _ json.RawMessage, info tools.ExecInfo) (tools.Reconciled, error) {
	if p.recon != nil {
		return p.recon(info)
	}
	return tools.Reconciled{State: tools.ReconUnknown}, nil
}

type fixture struct {
	t      *testing.T
	st     store.Store
	clk    *clock
	fake   *fakeocis.Server
	env    tools.Env
	reg    *tools.Registry
	gate   *Gate
	ex     *Executor
	probes map[string]*probe
}

// newFixture builds a gate and an executor over real tools plus four
// scripted probes (probe.read, probe.auto, probe.write, probe.destroy), a
// SQLite store whose clock is the fixture clock and a fake oCIS.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := newClock()
	fake := fakeocis.New(t)
	fake.AddUser("einstein", "pw", userID, drive)
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"), sqlite.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InTx(bg, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: userID, Name: "einstein", PersonalDriveID: drive})
	}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, clk: clk, fake: fake, probes: map[string]*probe{}}
	all := tools.All()
	for _, p := range []*probe{
		{name: "probe.read", risk: tools.RiskRead},
		{name: "probe.auto", risk: tools.RiskWrite, auto: true},
		{name: "probe.write", risk: tools.RiskWrite},
		{name: "probe.destroy", risk: tools.RiskDestructive},
	} {
		f.probes[p.name] = p
		all = append(all, p)
	}
	f.reg, err = tools.NewRegistry(all...)
	if err != nil {
		t.Fatal(err)
	}
	f.env = tools.Env{
		UserID: userID, DriveID: drive, Creds: ocisclient.AppToken("einstein", "pw"),
		Files: client, Memory: memory.New(client, st, memory.Limits{}),
	}
	cfg := Config{Now: clk.Now}
	if f.gate, err = NewGate(st, f.reg, cfg); err != nil {
		t.Fatal(err)
	}
	if f.ex, err = NewExecutor(st, f.reg, cfg); err != nil {
		t.Fatal(err)
	}
	return f
}

// claim inserts a job for the user and claims it, like a worker would.
func (f *fixture) claim() store.Claim {
	f.t.Helper()
	var c store.Claim
	err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.InsertJob(store.Job{UserID: userID, TriggerKind: "chat", Goal: "g"}); err != nil {
			return err
		}
		var ok bool
		var err error
		c, ok, err = tx.ClaimNextJob("w1", 2*time.Minute)
		if err == nil && !ok {
			f.t.Fatal("nothing to claim")
		}
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func call(id, tool, args string) Call {
	return Call{ID: id, Tool: tool, Args: json.RawMessage(args), Rationale: "because"}
}

func (f *fixture) classify(c store.Claim, cl Call) Decision {
	f.t.Helper()
	d, err := f.gate.Classify(bg, c, f.env, cl)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *fixture) approvals(jobID string) []store.Approval {
	f.t.Helper()
	var out []store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		out, err = tx.ListApprovals(userID, store.ApprovalFilter{JobID: jobID, Limit: 500})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) jobState(jobID string) store.JobState {
	f.t.Helper()
	var j store.Job
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(userID, jobID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return j.State
}

func (f *fixture) records() []audit.Record {
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

func (f *fixture) kinds() string {
	var k []string
	for _, r := range f.records() {
		k = append(k, r.Kind)
	}
	return strings.Join(k, ",")
}

func (f *fixture) steps(jobID string) []store.JobStep {
	f.t.Helper()
	var s []store.JobStep
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		s, err = tx.ListSteps(userID, jobID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) idem(key string) (store.IdemRecord, error) {
	var r store.IdemRecord
	err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		r, err = tx.GetIdempotent(userID, key)
		return err
	})
	return r, err
}
