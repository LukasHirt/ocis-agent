package approvals

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// approve decides the approvals as approved and claims the woken job again
// (the next fence), like the runner does after an approval.
func (f *fixture) approve(as ...store.Approval) store.Claim {
	f.t.Helper()
	var items []DecisionItem
	for _, a := range as {
		items = append(items, item(a, store.ApprovalApproved))
	}
	if _, err := f.gate.Decide(bg, userID, userID, items); err != nil {
		f.t.Fatal(err)
	}
	return f.reclaim()
}

func (f *fixture) approvalOf(id string) store.Approval {
	f.t.Helper()
	var a store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(userID, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestRunExecutesAnApprovedCallOnceAndRecordsEverything(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{"n":1}`))
	c := f.approve(as[0])
	out, err := f.ex.Run(bg, c, f.env, as[0].ID)
	if err != nil || out.State != OutcomeDone || out.Result.Summary != "probe ran" || out.Reconciled {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	p := f.probes["probe.write"]
	if p.execs.Load() != 1 || p.lastInfo().IdemKey != as[0].IdemKey || p.lastInfo().TargetETag != "\"t1\"" {
		t.Fatalf("execs %d last %+v", p.execs.Load(), p.lastInfo())
	}
	a := f.approvalOf(as[0].ID)
	if a.State != store.ApprovalExecuted || a.ExecutedAt == nil || !strings.Contains(string(a.Result), `"outcome":"done"`) {
		t.Fatalf("approval = %+v", a)
	}
	rec, err := f.idem(as[0].IdemKey)
	if err != nil || rec.State != store.IdemDone || !strings.Contains(string(rec.Result), "probe ran") {
		t.Fatalf("idempotency record = %+v %v", rec, err)
	}
	recs := f.records()
	last := recs[len(recs)-1]
	if last.Kind != "action_executed" || last.Tool != "probe.write" || last.Risk != "write" || last.ArgsHash != as[0].ArgsHash || last.Cred != "app" || last.Outcome != "done" || last.Detail != "probe ran" || last.JobID != c.Job.ID {
		t.Fatalf("audit record = %+v", last)
	}
	steps := f.steps(c.Job.ID)
	if len(steps) != 1 || steps[0].Kind != "tool" || steps[0].Tool != "probe.write" || steps[0].CallID != "c1" || steps[0].Status != "done" || steps[0].ApprovalID != as[0].ID || steps[0].Summary != "probe ran" {
		t.Fatalf("steps = %+v", steps)
	}

	// running it again returns the stored outcome and does nothing
	again, err := f.ex.Run(bg, c, f.env, as[0].ID)
	if err != nil || again.State != OutcomeDone || !again.Reconciled || again.Result.Summary != "probe ran" {
		t.Fatalf("second run = %+v %v", again, err)
	}
	if p.execs.Load() != 1 || len(f.steps(c.Job.ID)) != 1 {
		t.Fatal("a second run must not execute or record anything again")
	}
}

func TestAnOverwriteOfARealFileFollowsTheApprovalAndRefusesAChangedTarget(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/a.md", []byte("old"))
	f.fake.PutFile(drive, "/docs/b.md", []byte("old"))
	_, as := f.requested(
		call("c1", "files.overwrite", `{"path":"/docs/a.md","content":"agent a"}`),
		call("c2", "files.overwrite", `{"path":"/docs/b.md","content":"agent b"}`),
	)
	f.fake.PutFile(drive, "/docs/b.md", []byte("the user's edit")) // after the approval request
	cl := f.approve(as...)
	out, err := f.ex.Run(bg, cl, f.env, as[0].ID)
	if err != nil || out.State != OutcomeDone {
		t.Fatalf("a: %+v %v", out, err)
	}
	if data, _, _ := f.fake.File(drive, "/docs/a.md"); string(data) != "agent a" {
		t.Fatalf("a = %q", data)
	}
	out, err = f.ex.Run(bg, cl, f.env, as[1].ID)
	if err != nil || out.State != OutcomeFailed || !strings.Contains(out.Reason, "changed") {
		t.Fatalf("b: %+v %v", out, err)
	}
	if data, _, _ := f.fake.File(drive, "/docs/b.md"); string(data) != "the user's edit" {
		t.Fatalf("b = %q: the user's edit must survive", data)
	}
	if f.approvalOf(as[1].ID).State != store.ApprovalFailed {
		t.Fatal("the approval must be failed")
	}
}

func TestFailureKindsAreMappedAndNothingIsRetried(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want OutcomeState
	}{
		"refused":        {fmt.Errorf("it exists: %w", tools.ErrRefused), OutcomeFailed},
		"invalid":        {fmt.Errorf("bad: %w", tools.ErrInvalidArgs), OutcomeFailed},
		"target changed": {fmt.Errorf("moved: %w", tools.ErrTargetChanged), OutcomeFailed},
		"network":        {errors.New("connection reset"), OutcomeUnknown},
		"unauthorized":   {fmt.Errorf("stat: %w", ocisclient.ErrUnauthorized), OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			p.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, tc.err }
			_, as := f.requested(call("c1", "probe.write", `{}`))
			c := f.approve(as[0])
			out, err := f.ex.Run(bg, c, f.env, as[0].ID)
			if err != nil || out.State != tc.want {
				t.Fatalf("outcome = %+v %v, want %v", out, err, tc.want)
			}
			if tc.want == OutcomeFailed && out.Reason != tc.err.Error() {
				t.Fatalf("reason = %q", out.Reason)
			}
			if tc.want == OutcomeUnknown {
				if !errors.Is(out.Err, tc.err) || strings.Contains(out.Reason, "reset") || strings.Contains(out.Reason, "tools:") {
					t.Fatalf("unknown outcome: err %v reason %q (the cause must not reach the model)", out.Err, out.Reason)
				}
				if f.approvalOf(as[0].ID).State != store.ApprovalUnknown {
					t.Fatal("the approval must be unknown")
				}
				rec, _ := f.idem(as[0].IdemKey)
				if rec.State != store.IdemFailed || !strings.Contains(string(rec.Result), `"outcome":"unknown"`) {
					t.Fatalf("record = %+v", rec)
				}
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d, an unknown outcome must not be retried", p.execs.Load())
			}
		})
	}
}

// insertRaw stores an approval directly, bypassing the gate, and approves it.
func (f *fixture) insertRaw(c store.Claim, a store.Approval) store.Approval {
	f.t.Helper()
	a.UserID, a.JobID = userID, c.Job.ID
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = f.clk.Now().Add(time.Hour)
	}
	var out store.Approval
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		var err error
		out, err = tx.InsertApproval(a, c.Fence)
		if err != nil {
			return err
		}
		out, err = tx.DecideApproval(userID, out.ID, store.ApprovalApproved, userID, out.ArgsHash)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestTamperedApprovalsAreRefusedBeforeAnyExternalCall(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	hash, _ := tools.ArgsHash(json.RawMessage(`{"n":1}`))
	good := store.Approval{CallID: "c1", Tool: "probe.write", Risk: "write", Args: json.RawMessage(`{"n":1}`), ArgsHash: hash, IdemKey: tools.IdemKey(c.Job.ID, "c1", hash)}
	cases := map[string]func(a *store.Approval){
		"args do not match the hash": func(a *store.Approval) { a.Args = json.RawMessage(`{"n":2}`) },
		"wrong idempotency key":      func(a *store.Approval) { a.IdemKey = "other:c1:" + strings.TrimPrefix(hash, "sha256:") },
		"risk lowered":               func(a *store.Approval) { a.Tool, a.Risk = "probe.destroy", "write" },
		"unknown tool":               func(a *store.Approval) { a.Tool = "shell.exec" },
	}
	i := 0
	for name, mutate := range cases {
		i++
		a := good
		a.CallID = fmt.Sprintf("c%d", i)
		a.IdemKey = tools.IdemKey(c.Job.ID, a.CallID, hash)
		mutate(&a)
		stored := f.insertRaw(c, a)
		if _, err := f.ex.Run(bg, c, f.env, stored.ID); !errors.Is(err, ErrTampered) {
			t.Errorf("%s: err = %v, want ErrTampered", name, err)
		}
	}
	for _, p := range f.probes {
		if p.execs.Load() != 0 {
			t.Fatalf("%s executed despite tampering", p.name)
		}
	}
	for _, a := range f.approvals(c.Job.ID) {
		if a.State != store.ApprovalApproved {
			t.Fatalf("approval %s moved to %s", a.ID, a.State)
		}
	}
}

func TestAnApprovalOfAnotherJobOrANonApprovedOneIsRefused(t *testing.T) {
	f := newFixture(t)
	c1, as := f.requested(call("c1", "probe.write", `{}`))
	// pending: not approved yet (checked before anything is written)
	if _, err := f.ex.Run(bg, c1, f.env, as[0].ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("pending approval: %v, want ErrNotApproved", err)
	}
	// job 1 sleeps, so the user's lease is free and a second job can run
	c2 := f.claim()
	if c2.Job.ID == c1.Job.ID {
		t.Fatal("expected a second job")
	}
	if _, err := f.ex.Run(bg, c2, f.env, as[0].ID); !errors.Is(err, ErrWrongJob) {
		t.Fatalf("approval of another job: %v, want ErrWrongJob", err)
	}
	if _, err := f.ex.Run(bg, c2, f.env, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown approval: %v", err)
	}
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalRejected)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ex.Run(bg, c1, f.env, as[0].ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("rejected approval: %v, want ErrNotApproved", err)
	}
	if f.probes["probe.write"].execs.Load() != 0 {
		t.Fatal("nothing may run")
	}
}

func TestAnApprovedCallThatExpiredIsNotRun(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	f.clk.Advance(169 * time.Hour)
	out, err := f.ex.Run(bg, c, f.env, as[0].ID)
	if err != nil || out.State != OutcomeExpired || out.Reason == "" {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	if f.probes["probe.write"].execs.Load() != 0 {
		t.Fatal("an expired approval must not execute")
	}
	if f.approvalOf(as[0].ID).State != store.ApprovalExpired {
		t.Fatal("the approval must be expired")
	}
	if k := f.kinds(); !strings.HasSuffix(k, "approval_expired") {
		t.Fatalf("audit = %s", k)
	}
	if st := f.steps(c.Job.ID); len(st) != 1 || st[0].Status != "expired" {
		t.Fatalf("steps = %+v", st)
	}
	again, err := f.ex.Run(bg, c, f.env, as[0].ID)
	if err != nil || again.State != OutcomeExpired {
		t.Fatalf("second run = %+v %v", again, err)
	}
}

func TestAStaleLeaseStopsTransactionAWithoutChangingAnything(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	stale := c
	stale.Fence--
	if _, err := f.ex.Run(bg, stale, f.env, as[0].ID); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("err = %v, want ErrFenceLost", err)
	}
	if f.probes["probe.write"].execs.Load() != 0 {
		t.Fatal("a stale worker must not execute")
	}
	if f.approvalOf(as[0].ID).State != store.ApprovalApproved {
		t.Fatal("transaction A must roll back as a whole")
	}
	if _, err := f.idem(as[0].IdemKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no idempotency record may exist: %v", err)
	}
}

func TestRunAutoExecutesAMemoryStyleWriteUnderTheSameProtocol(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	cl := call("c1", "probe.auto", `{"n":1}`)
	d := f.classify(c, cl)
	if d.Kind != KindAllow {
		t.Fatalf("decision = %+v", d)
	}
	out, err := f.ex.RunAuto(bg, c, f.env, d, cl)
	if err != nil || out.State != OutcomeDone {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	rec, err := f.idem(d.IdemKey)
	if err != nil || rec.State != store.IdemDone {
		t.Fatalf("record = %+v %v", rec, err)
	}
	if k := f.kinds(); k != "action_executed" {
		t.Fatalf("audit = %s", k)
	}
	again, err := f.ex.RunAuto(bg, c, f.env, d, cl)
	if err != nil || !again.Reconciled || again.State != OutcomeDone || f.probes["probe.auto"].execs.Load() != 1 {
		t.Fatalf("second run = %+v %v (execs %d)", again, err, f.probes["probe.auto"].execs.Load())
	}
}

func TestRunAutoRefusesWhatIsNotAutoAllowed(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	for _, tc := range []struct{ tool, args string }{{"probe.write", `{}`}, {"probe.destroy", `{}`}, {"probe.read", `{}`}} {
		cl := call("c"+tc.tool[6:7], tc.tool, tc.args)
		d := f.classify(c, cl)
		if _, err := f.ex.RunAuto(bg, c, f.env, d, cl); !errors.Is(err, ErrBadCall) {
			t.Errorf("%s: err = %v, want ErrBadCall", tc.tool, err)
		}
	}
	// a decision whose arguments were altered after classification
	cl := call("cx", "probe.auto", `{"n":1}`)
	d := f.classify(c, cl)
	d.Args = json.RawMessage(`{"n":2}`)
	if _, err := f.ex.RunAuto(bg, c, f.env, d, cl); !errors.Is(err, ErrTampered) {
		t.Fatalf("altered args: %v, want ErrTampered", err)
	}
	for _, p := range f.probes {
		if p.execs.Load() != 0 {
			t.Fatalf("%s executed", p.name)
		}
	}
}

func TestRunReadAuditsRecordsAndSeparatesRefusalsFromFailures(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	cl := call("c1", "probe.read", `{}`)
	d := f.classify(c, cl)
	out, err := f.ex.RunRead(bg, c, f.env, d, cl)
	if err != nil || out.State != OutcomeDone {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	if k := f.kinds(); k != "tool_call" {
		t.Fatalf("audit = %s", k)
	}
	if st := f.steps(c.Job.ID); len(st) != 1 || st[0].Tool != "probe.read" || st[0].Status != "done" || st[0].ApprovalID != "" {
		t.Fatalf("steps = %+v", st)
	}
	// a refusal is a failed outcome the model reads
	f.probes["probe.read"].run = func(tools.ExecInfo) (tools.Result, error) {
		return tools.Result{}, fmt.Errorf("no such file: %w", tools.ErrRefused)
	}
	cl2 := call("c2", "probe.read", `{"n":2}`)
	out, err = f.ex.RunRead(bg, c, f.env, f.classify(c, cl2), cl2)
	if err != nil || out.State != OutcomeFailed || out.Reason != "no such file: tools: refused" {
		t.Fatalf("refusal = %+v %v", out, err)
	}
	// anything else is returned to the runner and nothing is recorded
	f.probes["probe.read"].run = func(tools.ExecInfo) (tools.Result, error) {
		return tools.Result{}, fmt.Errorf("stat: %w", ocisclient.ErrUnauthorized)
	}
	before := len(f.steps(c.Job.ID))
	cl3 := call("c3", "probe.read", `{"n":3}`)
	if _, err := f.ex.RunRead(bg, c, f.env, f.classify(c, cl3), cl3); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("err = %v, want the 401 returned", err)
	}
	if len(f.steps(c.Job.ID)) != before {
		t.Fatal("a failed read must not record a step")
	}
	// only reads
	clw := call("c4", "probe.write", `{}`)
	if _, err := f.ex.RunRead(bg, c, f.env, f.classify(c, clw), clw); !errors.Is(err, ErrBadCall) {
		t.Fatalf("a write through RunRead: %v, want ErrBadCall", err)
	}
	// a stale lease cannot record steps
	stale := c
	stale.Fence--
	f.probes["probe.read"].run = nil
	cl5 := call("c5", "probe.read", `{"n":5}`)
	if _, err := f.ex.RunRead(bg, stale, f.env, f.classify(c, cl5), cl5); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("stale fence: %v, want ErrFenceLost", err)
	}
}
