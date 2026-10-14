package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Execution", runExecution) }

var errInjected = errors.New("injected failure")

// beginExecution is transaction A of the execution protocol.
func beginExecution(tx store.Tx, c store.Claim, a store.Approval) (*store.IdemRecord, error) {
	if err := tx.AssertLease(c.Job.UserID, c.Job.ID, c.Fence); err != nil {
		return nil, err
	}
	if _, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{}); err != nil {
		return nil, err
	}
	return tx.BeginIdempotent(store.IdemRecord{Key: a.IdemKey, UserID: a.UserID, Tool: a.Tool})
}

// finishExecution is transaction B. failAfterFinish injects a failure after the
// first writes to prove the transaction is all-or-nothing.
func finishExecution(tx store.Tx, c store.Claim, a store.Approval, result string, failAfterFinish bool) error {
	if err := tx.AssertLease(c.Job.UserID, c.Job.ID, c.Fence); err != nil {
		return err
	}
	if err := tx.FinishIdempotent(a.UserID, a.IdemKey, store.IdemDone, json.RawMessage(result)); err != nil {
		return err
	}
	if _, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalExecuting, store.ApprovalExecuted, store.ApprovalPatch{Result: json.RawMessage(result)}); err != nil {
		return err
	}
	if failAfterFinish {
		return errInjected
	}
	if _, err := tx.AppendAudit(audit.Event{
		UserID: a.UserID, JobID: a.JobID, Kind: "action_executed", Tool: a.Tool, Risk: a.Risk,
		ArgsHash: a.ArgsHash, Cred: "app", Outcome: "ok",
	}); err != nil {
		return err
	}
	_, err := tx.AppendStep(store.JobStep{
		JobID: a.JobID, Kind: "tool", Tool: a.Tool, CallID: a.CallID, Status: "ok", ApprovalID: a.ID, Summary: "executed",
	}, c.Fence)
	return err
}

// approvedRun takes a job through the whole approval flow and returns the
// second claim (the run that executes the approved action) and the approval.
func approvedRun(t *testing.T, st store.Store, clk *Clock) (store.Claim, store.Approval) {
	t.Helper()
	c1 := runningFor(t, st, "u1")
	a := waitFor(t, st, clk, c1, 1)[0]
	if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
		t.Fatal(err)
	}
	if !resume(t, st, "u1", c1.Job.ID) {
		t.Fatal("the job did not resume")
	}
	return mustClaim(t, st, "w2"), a
}

func countAudit(t *testing.T, st store.Store, user, kind string) int {
	t.Helper()
	n := 0
	err := st.View(bg(), func(tx store.Tx) error {
		recs, err := tx.ReadAudit(user, 0, 1000)
		for _, r := range recs {
			if r.Kind == kind {
				n++
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func stepCount(t *testing.T, st store.Store, user, job string) int {
	t.Helper()
	var n int
	err := st.View(bg(), func(tx store.Tx) error {
		steps, err := tx.ListSteps(user, job)
		n = len(steps)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func runExecution(t *testing.T, f Factory) {
	t.Run("transactions A and B move everything together", func(t *testing.T) {
		st, clk := open(t, f)
		c, a := approvedRun(t, st, clk)

		mustTx(t, st, func(tx store.Tx) error {
			existing, err := beginExecution(tx, c, a)
			if existing != nil {
				t.Errorf("a first execution found an existing record: %+v", existing)
			}
			return err
		})
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalExecuting {
			t.Fatalf("after A the approval is %s", got.State)
		}
		if got := getKey(t, st, "u1", a.IdemKey); got.State != store.IdemStarted {
			t.Fatalf("after A the idempotency record is %s", got.State)
		}

		mustTx(t, st, func(tx store.Tx) error { return finishExecution(tx, c, a, `{"deleted":true}`, false) })
		done := getApproval(t, st, "u1", a.ID)
		if done.State != store.ApprovalExecuted || string(done.Result) != `{"deleted":true}` || done.ExecutedAt == nil {
			t.Fatalf("after B the approval is %+v", done)
		}
		if rec := getKey(t, st, "u1", a.IdemKey); rec.State != store.IdemDone || string(rec.Result) != `{"deleted":true}` {
			t.Fatalf("after B the idempotency record is %+v", rec)
		}
		if n := countAudit(t, st, "u1", "action_executed"); n != 1 {
			t.Fatalf("audit events = %d, want 1", n)
		}
		if n := stepCount(t, st, "u1", c.Job.ID); n != 1 {
			t.Fatalf("steps = %d, want 1", n)
		}
	})

	t.Run("a crash between A and B leaves a started record that a retry finds", func(t *testing.T) {
		st, clk := open(t, f)
		c, a := approvedRun(t, st, clk)
		mustTx(t, st, func(tx store.Tx) error { _, err := beginExecution(tx, c, a); return err })

		// the worker dies here, before B. A restarted worker must see the
		// started record instead of running the call again.
		var existing *store.IdemRecord
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			existing, err = tx.BeginIdempotent(store.IdemRecord{Key: a.IdemKey, UserID: "u1", Tool: a.Tool})
			return err
		})
		if existing == nil || existing.State != store.IdemStarted {
			t.Fatalf("restart found %+v, want the started record", existing)
		}
		var stale []store.IdemRecord
		clk.Advance(5 * time.Minute)
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			stale, err = tx.ListStartedIdempotent(clk.Now(), 0)
			return err
		}); err != nil || len(stale) != 1 || stale[0].Key != a.IdemKey {
			t.Fatalf("the reconciler's view = %+v, %v", stale, err)
		}

		// running transaction A again must fail without touching anything
		err := st.InTx(bg(), func(tx store.Tx) error { _, err := beginExecution(tx, c, a); return err })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second A: err = %v, want ErrConflict (approval is already executing)", err)
		}
		// after reconciling, the worker completes with B
		mustTx(t, st, func(tx store.Tx) error { return finishExecution(tx, c, a, `{"deleted":true}`, false) })
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalExecuted {
			t.Fatalf("approval = %s", got.State)
		}
	})

	t.Run("transaction B is all or nothing", func(t *testing.T) {
		st, clk := open(t, f)
		c, a := approvedRun(t, st, clk)
		mustTx(t, st, func(tx store.Tx) error { _, err := beginExecution(tx, c, a); return err })

		err := st.InTx(bg(), func(tx store.Tx) error { return finishExecution(tx, c, a, `{"deleted":true}`, true) })
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the injected failure", err)
		}
		if got := getKey(t, st, "u1", a.IdemKey); got.State != store.IdemStarted || len(got.Result) != 0 {
			t.Errorf("idempotency record = %+v, want it untouched", got)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalExecuting || len(got.Result) != 0 {
			t.Errorf("approval = %+v, want it still executing", got)
		}
		if n := countAudit(t, st, "u1", "action_executed"); n != 0 {
			t.Errorf("audit events = %d, want 0", n)
		}
		if n := stepCount(t, st, "u1", c.Job.ID); n != 0 {
			t.Errorf("steps = %d, want 0", n)
		}
	})

	t.Run("a stale worker's transaction A changes nothing", func(t *testing.T) {
		st, clk := open(t, f)
		c, a := approvedRun(t, st, clk)
		addJob(t, st, "u1", "another job")
		clk.Advance(3 * time.Minute) // c's lease expires
		takeover := mustClaim(t, st, "w3")
		if takeover.Job.ID == c.Job.ID {
			t.Fatalf("the takeover claimed the stale job again")
		}

		err := st.InTx(bg(), func(tx store.Tx) error { _, err := beginExecution(tx, c, a); return err })
		if !errors.Is(err, store.ErrFenceLost) {
			t.Fatalf("err = %v, want ErrFenceLost", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalApproved {
			t.Errorf("approval = %s, want it still approved", got.State)
		}
		err = st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetIdempotent("u1", a.IdemKey)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an idempotency record exists after a refused A: %v", err)
		}
	})

	t.Run("a finished execution is not run again", func(t *testing.T) {
		st, clk := open(t, f)
		c, a := approvedRun(t, st, clk)
		mustTx(t, st, func(tx store.Tx) error { _, err := beginExecution(tx, c, a); return err })
		mustTx(t, st, func(tx store.Tx) error { return finishExecution(tx, c, a, `{"deleted":true}`, false) })

		var existing *store.IdemRecord
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			existing, err = tx.BeginIdempotent(store.IdemRecord{Key: a.IdemKey, UserID: "u1", Tool: a.Tool})
			return err
		})
		if existing == nil || existing.State != store.IdemDone || string(existing.Result) != `{"deleted":true}` {
			t.Fatalf("existing = %+v, want the done record with its result", existing)
		}
	})
}
