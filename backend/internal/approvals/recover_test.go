package approvals

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

var errCrash = errors.New("simulated crash")

// crashAt makes the executor fail at a boundary, like a killed process.
func (f *fixture) crashAt(stage string) {
	f.ex.hook = func(s string) error {
		if s == stage {
			return errCrash
		}
		return nil
	}
}

// restart simulates a worker restart: the lease expires, the reaper requeues
// the job and a new worker claims it with the next fence.
func (f *fixture) restart() store.Claim {
	f.t.Helper()
	f.ex.hook = nil
	f.clk.Advance(3 * time.Minute)
	var c store.Claim
	err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.ReapExpired(3, func(int) time.Duration { return 0 }); err != nil {
			return err
		}
		var ok bool
		var err error
		c, ok, err = tx.ClaimNextJob("w2", 2*time.Minute)
		if err == nil && !ok {
			f.t.Fatal("the reaped job was not claimable")
		}
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func approvedWrite(t *testing.T, f *fixture, p *probe) (store.Claim, store.Approval) {
	t.Helper()
	_, as := f.requested(call("c1", p.name, `{"n":1}`))
	return f.approve(as[0]), as[0]
}

func TestCrashAfterTransactionAReconcilesInsteadOfRunningBlindly(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict    tools.Reconciled
		want       OutcomeState
		wantExecs  int32
		wantState  store.ApprovalState
		reconciled bool // the outcome came from the tool's verdict, not from a fresh call
	}{
		"the tool says done":   {tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "already there"}}, OutcomeDone, 0, store.ApprovalExecuted, true},
		"the tool says failed": {tools.Reconciled{State: tools.ReconFailed, Reason: "conflict"}, OutcomeFailed, 0, store.ApprovalFailed, true},
		"the tool says retry":  {tools.Reconciled{State: tools.ReconRetry}, OutcomeDone, 1, store.ApprovalExecuted, false},
		"the tool cannot tell": {tools.Reconciled{State: tools.ReconUnknown}, OutcomeUnknown, 0, store.ApprovalUnknown, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tc.verdict, nil }
			c, a := approvedWrite(t, f, p)
			f.crashAt("afterA")
			if _, err := f.ex.Run(bg, c, f.env, a.ID); !errors.Is(err, errCrash) {
				t.Fatalf("err = %v, want the simulated crash", err)
			}
			if f.approvalOf(a.ID).State != store.ApprovalExecuting {
				t.Fatalf("approval state = %s, want executing", f.approvalOf(a.ID).State)
			}
			if rec, err := f.idem(a.IdemKey); err != nil || rec.State != store.IdemStarted {
				t.Fatalf("record = %+v %v, want started", rec, err)
			}
			if p.execs.Load() != 0 {
				t.Fatal("nothing ran yet")
			}

			c2 := f.restart()
			out, err := f.ex.Run(bg, c2, f.env, a.ID)
			if err != nil || out.State != tc.want || out.Reconciled != tc.reconciled {
				t.Fatalf("outcome = %+v %v, want %v (reconciled %v)", out, err, tc.want, tc.reconciled)
			}
			if p.execs.Load() != tc.wantExecs {
				t.Fatalf("execs = %d, want %d", p.execs.Load(), tc.wantExecs)
			}
			if got := f.approvalOf(a.ID).State; got != tc.wantState {
				t.Fatalf("approval state = %s, want %s", got, tc.wantState)
			}
			if rec, _ := f.idem(a.IdemKey); rec.State == store.IdemStarted {
				t.Fatal("the record must be finished")
			}
		})
	}
}

func TestCrashAfterTheExternalCallNeverRunsItTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict tools.Reconciled
		want    OutcomeState
	}{
		"the tool sees the effect": {tools.Reconciled{State: tools.ReconDone}, OutcomeDone},
		"the tool cannot tell":     {tools.Reconciled{State: tools.ReconUnknown}, OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tc.verdict, nil }
			c, a := approvedWrite(t, f, p)
			f.crashAt("afterExecute")
			if _, err := f.ex.Run(bg, c, f.env, a.ID); !errors.Is(err, errCrash) {
				t.Fatalf("err = %v", err)
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d", p.execs.Load())
			}
			c2 := f.restart()
			out, err := f.ex.Run(bg, c2, f.env, a.ID)
			if err != nil || out.State != tc.want {
				t.Fatalf("outcome = %+v %v", out, err)
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d after recovery: a call that ran must never run again", p.execs.Load())
			}
		})
	}
}

func TestAnAnswerThatWasCommittedButLostIsReturnedWithoutRerunning(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	c, a := approvedWrite(t, f, p)
	if _, err := f.ex.Run(bg, c, f.env, a.ID); err != nil {
		t.Fatal(err)
	}
	before := f.kinds()
	c2 := f.restart() // the worker died after B; the job is run again
	out, err := f.ex.Run(bg, c2, f.env, a.ID)
	if err != nil || out.State != OutcomeDone || !out.Reconciled {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	if p.execs.Load() != 1 || f.kinds() != before {
		t.Fatalf("a replay must do and record nothing (execs %d, audit %s -> %s)", p.execs.Load(), before, f.kinds())
	}
}

func TestAnUnknownErrorIsRetriedAtMostOnceAndOnlyWhenTheToolSaysSo(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	calls := 0
	p.run = func(tools.ExecInfo) (tools.Result, error) {
		calls++
		if calls == 1 {
			return tools.Result{}, errors.New("connection reset")
		}
		return tools.Result{Summary: "second time lucky"}, nil
	}
	p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tools.Reconciled{State: tools.ReconRetry}, nil }
	c, a := approvedWrite(t, f, p)
	out, err := f.ex.Run(bg, c, f.env, a.ID)
	if err != nil || out.State != OutcomeDone || out.Result.Summary != "second time lucky" || p.execs.Load() != 2 {
		t.Fatalf("outcome = %+v %v execs %d", out, err, p.execs.Load())
	}

	// a tool that keeps failing: exactly two executions, then unknown
	f2 := newFixture(t)
	p2 := f2.probes["probe.write"]
	p2.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, errors.New("still down") }
	p2.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tools.Reconciled{State: tools.ReconRetry}, nil }
	c2, a2 := approvedWrite(t, f2, p2)
	out, err = f2.ex.Run(bg, c2, f2.env, a2.ID)
	if err != nil || out.State != OutcomeUnknown || p2.execs.Load() != 2 {
		t.Fatalf("outcome = %+v %v execs %d, want unknown after two executions", out, err, p2.execs.Load())
	}

	// the tool says it already happened: no second execution
	f3 := newFixture(t)
	p3 := f3.probes["probe.write"]
	p3.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, errors.New("timeout") }
	p3.recon = func(tools.ExecInfo) (tools.Reconciled, error) {
		return tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "it did happen"}}, nil
	}
	c3, a3 := approvedWrite(t, f3, p3)
	out, err = f3.ex.Run(bg, c3, f3.env, a3.ID)
	if err != nil || out.State != OutcomeDone || !out.Reconciled || p3.execs.Load() != 1 {
		t.Fatalf("outcome = %+v %v execs %d", out, err, p3.execs.Load())
	}

	// a reconcile that itself fails means unknown, with both causes kept for the runner
	f4 := newFixture(t)
	p4 := f4.probes["probe.write"]
	boom, cannot := errors.New("boom"), errors.New("cannot check")
	p4.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, boom }
	p4.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tools.Reconciled{}, cannot }
	c4, a4 := approvedWrite(t, f4, p4)
	out, err = f4.ex.Run(bg, c4, f4.env, a4.ID)
	if err != nil || out.State != OutcomeUnknown || !errors.Is(out.Err, boom) || !errors.Is(out.Err, cannot) || p4.execs.Load() != 1 {
		t.Fatalf("outcome = %+v %v", out, err)
	}
}

func TestAnUnknownOutcomeIsStoredAndNotRerun(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	p.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, errors.New("reset") }
	c, a := approvedWrite(t, f, p)
	if out, err := f.ex.Run(bg, c, f.env, a.ID); err != nil || out.State != OutcomeUnknown {
		t.Fatalf("first run = %+v %v", out, err)
	}
	out, err := f.ex.Run(bg, c, f.env, a.ID)
	if err != nil || out.State != OutcomeUnknown || !out.Reconciled || p.execs.Load() != 1 {
		t.Fatalf("second run = %+v %v execs %d", out, err, p.execs.Load())
	}
}

func TestACancelledJobDoesNotLoseAKnownOutcome(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	c, a := approvedWrite(t, f, p)
	p.run = func(tools.ExecInfo) (tools.Result, error) {
		// the user cancels the job while the external call is running
		if err := f.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, c.Job.ID) }); err != nil {
			t.Error(err)
		}
		return tools.Result{Summary: "done anyway"}, nil
	}
	out, err := f.ex.Run(bg, c, f.env, a.ID)
	if err != nil || out.State != OutcomeDone {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	if f.approvalOf(a.ID).State != store.ApprovalExecuted {
		t.Fatal("the approval must record the executed action")
	}
	if rec, err := f.idem(a.IdemKey); err != nil || rec.State != store.IdemDone {
		t.Fatalf("record = %+v %v", rec, err)
	}
	if k := f.kinds(); k != "approval_requested,approval_decided,action_executed" {
		t.Fatalf("audit = %s", k)
	}
	if st := f.steps(c.Job.ID); len(st) != 0 {
		t.Fatalf("a cancelled job must not get a step: %+v", st)
	}
	if got := f.jobState(c.Job.ID); got != store.JobCancelled {
		t.Fatalf("job state = %s", got)
	}
}

func TestAStaleWorkerRecordsItsOutcomeButNotTheJobTimeline(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	c, a := approvedWrite(t, f, p)
	var taken store.Claim
	p.run = func(tools.ExecInfo) (tools.Result, error) {
		// while the call runs, the lease expires and another worker takes the job
		taken = f.restart()
		f.ex.hook = nil
		return tools.Result{Summary: "done by the stale worker"}, nil
	}
	out, err := f.ex.Run(bg, c, f.env, a.ID)
	if err != nil || out.State != OutcomeDone {
		t.Fatalf("outcome = %+v %v", out, err)
	}
	if taken.Fence <= c.Fence {
		t.Fatalf("the takeover must raise the fence (%d -> %d)", c.Fence, taken.Fence)
	}
	if f.approvalOf(a.ID).State != store.ApprovalExecuted {
		t.Fatal("the executed action must be on record")
	}
	if st := f.steps(c.Job.ID); len(st) != 0 {
		t.Fatalf("the stale worker must not write the job's timeline: %+v", st)
	}
	if got := f.jobState(c.Job.ID); got != store.JobRunning {
		t.Fatalf("job state = %s: the new holder's run must be untouched", got)
	}
	// the new holder, running the same approval, sees the finished outcome
	again, err := f.ex.Run(bg, taken, f.env, a.ID)
	if err != nil || again.State != OutcomeDone || p.execs.Load() != 1 {
		t.Fatalf("new holder: %+v %v execs %d", again, err, p.execs.Load())
	}
}

func TestAutoWritesRecoverLikeApprovedOnes(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.auto"]
	p.recon = func(tools.ExecInfo) (tools.Reconciled, error) {
		return tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "already written"}}, nil
	}
	c := f.claim()
	cl := call("m1", "probe.auto", `{"n":1}`)
	d := f.classify(c, cl)
	f.crashAt("afterExecute")
	if _, err := f.ex.RunAuto(bg, c, f.env, d, cl); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	c2 := f.restart()
	cl2 := call("m1", "probe.auto", `{"n":1}`) // the resumed job derives the same call id
	d2 := f.classify(c2, cl2)
	if d2.IdemKey != d.IdemKey {
		t.Fatalf("a resumed job must derive the same key: %q vs %q", d2.IdemKey, d.IdemKey)
	}
	out, err := f.ex.RunAuto(bg, c2, f.env, d2, cl2)
	if err != nil || out.State != OutcomeDone || !out.Reconciled || p.execs.Load() != 1 {
		t.Fatalf("outcome = %+v %v execs %d", out, err, p.execs.Load())
	}
}
