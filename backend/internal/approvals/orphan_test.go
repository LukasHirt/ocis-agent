package approvals

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func (f *fixture) envFor() EnvFunc {
	return func(context.Context, string) (tools.Env, error) { return f.env, nil }
}

func (f *fixture) cancel(jobID string) {
	f.t.Helper()
	if err := f.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, jobID) }); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) due() time.Time { return f.clk.Now().Add(time.Second) }

// crashedAndEnded leaves an approval executing and its record started because
// the worker died after the call, and then the job ends.
func crashedAndEnded(t *testing.T, f *fixture, p *probe) (store.Claim, store.Approval) {
	t.Helper()
	c, a := approvedWrite(t, f, p)
	f.crashAt("afterExecute")
	if _, err := f.ex.Run(bg, c, f.env, a.ID); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v", err)
	}
	f.ex.hook = nil
	f.cancel(c.Job.ID)
	return c, a
}

func TestOrphansOfEndedJobsAreResolvedByTheTool(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict tools.Reconciled
		err     error
		want    store.ApprovalState
		detail  string
	}{
		"done":    {verdict: tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "it is there"}}, want: store.ApprovalExecuted, detail: "reconciled: it is there"},
		"failed":  {verdict: tools.Reconciled{State: tools.ReconFailed, Reason: "conflict"}, want: store.ApprovalFailed, detail: "reconciled: conflict"},
		"retry":   {verdict: tools.Reconciled{State: tools.ReconRetry}, want: store.ApprovalFailed, detail: "not run"},
		"unknown": {verdict: tools.Reconciled{State: tools.ReconUnknown}, want: store.ApprovalUnknown, detail: "reconciled:"},
		"error":   {err: errors.New("cannot check"), want: store.ApprovalUnknown, detail: "reconciled:"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			p.recon = func(tools.ExecInfo) (tools.Reconciled, error) { return tc.verdict, tc.err }
			_, a := crashedAndEnded(t, f, p)
			sum, err := f.ex.ReconcileOrphans(bg, f.envFor(), f.due(), 100)
			if err != nil || sum.Resolved != 1 || sum.Skipped != 0 || len(sum.Errors) != 0 {
				t.Fatalf("summary = %+v %v", sum, err)
			}
			if got := f.approvalOf(a.ID).State; got != tc.want {
				t.Fatalf("approval state = %s, want %s", got, tc.want)
			}
			if rec, _ := f.idem(a.IdemKey); rec.State == store.IdemStarted {
				t.Fatal("the record must be finished")
			}
			recs := f.records()
			last := recs[len(recs)-1]
			if last.Kind != "action_executed" || !strings.Contains(last.Detail, tc.detail) {
				t.Fatalf("last audit = %+v, want detail containing %q", last, tc.detail)
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d: reconciling an ended job must never execute again", p.execs.Load())
			}
			// resolved once: a second pass finds nothing
			if again, _ := f.ex.ReconcileOrphans(bg, f.envFor(), f.due(), 100); again.Resolved != 0 {
				t.Fatalf("second pass resolved %d", again.Resolved)
			}
		})
	}
}

func TestOrphansOfActiveJobsAreLeftToTheJob(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	c, a := approvedWrite(t, f, p)
	f.crashAt("afterExecute")
	if _, err := f.ex.Run(bg, c, f.env, a.ID); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	f.ex.hook = nil
	sum, err := f.ex.ReconcileOrphans(bg, f.envFor(), f.due(), 100)
	if err != nil || sum.Resolved != 0 || sum.Skipped != 1 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	if rec, _ := f.idem(a.IdemKey); rec.State != store.IdemStarted || f.approvalOf(a.ID).State != store.ApprovalExecuting {
		t.Fatal("an active job's record must stay untouched")
	}
}

func TestOnlyOldEnoughRecordsAreConsidered(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	_, a := crashedAndEnded(t, f, p)
	sum, err := f.ex.ReconcileOrphans(bg, f.envFor(), f.clk.Now().Add(-time.Hour), 100)
	if err != nil || sum.Resolved != 0 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	if f.approvalOf(a.ID).State != store.ApprovalExecuting {
		t.Fatal("a young record must not be touched")
	}
}

func TestAnAutoAllowedOrphanIsFinishedAsUnknown(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.auto"]
	c := f.claim()
	cl := call("m1", "probe.auto", `{"n":1}`)
	d := f.classify(c, cl)
	f.crashAt("afterExecute")
	if _, err := f.ex.RunAuto(bg, c, f.env, d, cl); !errors.Is(err, errCrash) {
		t.Fatal(err)
	}
	f.ex.hook = nil
	f.cancel(c.Job.ID)
	sum, err := f.ex.ReconcileOrphans(bg, f.envFor(), f.due(), 100)
	if err != nil || sum.Resolved != 1 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	rec, err := f.idem(d.IdemKey)
	if err != nil || rec.State != store.IdemFailed || !strings.Contains(string(rec.Result), `"outcome":"unknown"`) {
		t.Fatalf("record = %+v %v", rec, err)
	}
	if p.execs.Load() != 1 {
		t.Fatal("nothing may run again")
	}
}

func TestAFailingEnvLeavesTheRecordAloneAndIsReported(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	_, a := crashedAndEnded(t, f, p)
	boom := errors.New("no credentials")
	sum, err := f.ex.ReconcileOrphans(bg, func(context.Context, string) (tools.Env, error) { return tools.Env{}, boom }, f.due(), 100)
	if err != nil || sum.Resolved != 0 || len(sum.Errors) != 1 || !errors.Is(sum.Errors[0], boom) {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	if rec, _ := f.idem(a.IdemKey); rec.State != store.IdemStarted || f.approvalOf(a.ID).State != store.ApprovalExecuting {
		t.Fatal("a failed attempt must leave the record untouched for the next pass")
	}
}
