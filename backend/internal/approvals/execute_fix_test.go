package approvals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func (f *fixture) move(a store.Approval, from, to store.ApprovalState) {
	f.t.Helper()
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.TransitionApproval(userID, a.ID, from, to, store.ApprovalPatch{})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) jobOf(a store.Approval) job {
	tool, _ := f.reg.Get(a.Tool)
	return job{tool: tool, callID: a.CallID, args: a.Args, argsHash: a.ArgsHash, idemKey: a.IdemKey, risk: tools.RiskWrite, approval: &a}
}

func TestBeginRefusesAnExecutingApprovalWithoutARecord(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	snap := f.approvalOf(as[0].ID) // approved
	f.move(snap, store.ApprovalApproved, store.ApprovalExecuting)
	if _, err := f.ex.begin(bg, c, f.jobOf(snap)); !errors.Is(err, ErrTampered) {
		t.Fatalf("err = %v, want ErrTampered", err)
	}
	if _, err := f.idem(snap.IdemKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no record may be left behind: %v", err)
	}
}

func TestBeginRefusesAnApprovalThatIsAlreadyFinal(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	snap := f.approvalOf(as[0].ID)
	f.move(snap, store.ApprovalApproved, store.ApprovalFailed)
	if _, err := f.ex.begin(bg, c, f.jobOf(snap)); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("err = %v, want ErrNotApproved", err)
	}
	if _, err := f.idem(snap.IdemKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no record may exist: %v", err)
	}
}

func TestBeginNormalPaths(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	snap := f.approvalOf(as[0].ID)
	rec, err := f.ex.begin(bg, c, f.jobOf(snap))
	if err != nil || rec != nil || f.approvalOf(snap.ID).State != store.ApprovalExecuting {
		t.Fatalf("first begin: %v %v", rec, err)
	}
	snap.State = store.ApprovalExecuting
	rec, err = f.ex.begin(bg, c, f.jobOf(snap))
	if err != nil || rec == nil || rec.State != store.IdemStarted {
		t.Fatalf("recovery begin: %v %v", rec, err)
	}
}

func TestACancelledContextLeavesTheCallOpenForReconciliation(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict tools.Reconciled
		want    OutcomeState
	}{
		"done":    {tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "found it"}}, OutcomeDone},
		"unknown": {tools.Reconciled{State: tools.ReconUnknown}, OutcomeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			ctx, cancel := context.WithCancel(bg)
			p.run = func(tools.ExecInfo) (tools.Result, error) { cancel(); return tools.Result{}, ctx.Err() }
			p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tc.verdict, nil }
			_, as := f.requested(call("c1", "probe.write", `{}`))
			c := f.approve(as[0])
			if _, err := f.ex.Run(ctx, c, f.env, as[0].ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if f.approvalOf(as[0].ID).State != store.ApprovalExecuting {
				t.Fatal("the approval must stay executing")
			}
			if rec, _ := f.idem(as[0].IdemKey); rec.State != store.IdemStarted {
				t.Fatalf("record = %+v, want started", rec)
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d", p.execs.Load())
			}
			out, err := f.ex.Run(bg, c, f.env, as[0].ID)
			if err != nil || out.State != tc.want || p.execs.Load() != 1 {
				t.Fatalf("second run = %+v %v (execs %d)", out, err, p.execs.Load())
			}
		})
	}
}

func TestARetryVerdictWaitsForTheSettleWindow(t *testing.T) {
	setup := func(f *fixture) (store.Claim, store.Approval) {
		p := f.probes["probe.write"]
		p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tools.Reconciled{State: tools.ReconRetry}, nil }
		_, as := f.requested(call("c1", "probe.write", `{}`))
		c := f.approve(as[0])
		f.ex.hook = func(stage string) error {
			if stage == "afterExecute" {
				return errors.New("crash")
			}
			return nil
		}
		if _, err := f.ex.Run(bg, c, f.env, as[0].ID); err == nil {
			t.Fatal("the simulated crash must surface")
		}
		f.ex.hook = nil
		return c, as[0]
	}
	t.Run("right after the crash", func(t *testing.T) {
		f := newFixture(t)
		c, a := setup(f)
		out, err := f.ex.Run(bg, c, f.env, a.ID)
		if err != nil || out.State != OutcomeUnknown || f.probes["probe.write"].execs.Load() != 1 {
			t.Fatalf("outcome = %+v %v (execs %d)", out, err, f.probes["probe.write"].execs.Load())
		}
	})
	t.Run("after the window", func(t *testing.T) {
		f := newFixture(t)
		c, a := setup(f)
		f.clk.Advance(f.ex.cfg.RetrySettle)
		out, err := f.ex.Run(bg, c, f.env, a.ID)
		if err != nil || out.State != OutcomeDone || f.probes["probe.write"].execs.Load() != 2 {
			t.Fatalf("outcome = %+v %v (execs %d)", out, err, f.probes["probe.write"].execs.Load())
		}
	})
}

func TestRunAutoAndRunReadRecheckTheirDecision(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	cl := call("c1", "probe.auto", `{"n":1}`)
	d := f.classify(c, cl)
	other := cl
	other.Tool = "probe.write"
	if _, err := f.ex.RunAuto(bg, c, f.env, d, other); !errors.Is(err, ErrBadCall) {
		t.Fatalf("another tool: %v, want ErrBadCall", err)
	}
	rl := call("c2", "probe.read", `{}`)
	rd := f.classify(c, rl)
	rd.Args = []byte(`{"n":2}`)
	if _, err := f.ex.RunRead(bg, c, f.env, rd, rl); !errors.Is(err, ErrTampered) {
		t.Fatalf("altered read args: %v, want ErrTampered", err)
	}
	for _, p := range f.probes {
		if p.execs.Load() != 0 {
			t.Fatalf("%s executed", p.name)
		}
	}
}

func TestReasonsShownToTheModelAreCapped(t *testing.T) {
	long := strings.Repeat("é", 1000)
	f := newFixture(t)
	p := f.probes["probe.write"]
	p.run = func(tools.ExecInfo) (tools.Result, error) {
		return tools.Result{}, fmt.Errorf("%s: %w", long, tools.ErrRefused)
	}
	_, as := f.requested(call("c1", "probe.write", `{}`))
	c := f.approve(as[0])
	out, err := f.ex.Run(bg, c, f.env, as[0].ID)
	if err != nil || out.State != OutcomeFailed || len(out.Reason) > 500 || !utf8.ValidString(out.Reason) || out.Reason == "" {
		t.Fatalf("outcome reason len %d valid %v err %v", len(out.Reason), utf8.ValidString(out.Reason), err)
	}

	f2 := newFixture(t)
	p2 := f2.probes["probe.write"]
	p2.run = func(tools.ExecInfo) (tools.Result, error) { return tools.Result{}, errors.New("boom") }
	p2.recon = func(tools.ExecInfo) (tools.Reconciled, error) {
		return tools.Reconciled{State: tools.ReconFailed, Reason: long}, nil
	}
	_, as2 := f2.requested(call("c1", "probe.write", `{}`))
	c2 := f2.approve(as2[0])
	out, err = f2.ex.Run(bg, c2, f2.env, as2[0].ID)
	if err != nil || out.State != OutcomeFailed || len(out.Reason) > 500 || !utf8.ValidString(out.Reason) {
		t.Fatalf("reconciled reason len %d err %v", len(out.Reason), err)
	}
	_ = time.Second
}
