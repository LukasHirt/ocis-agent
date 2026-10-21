package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Acknowledge", runAcknowledge) }

func acknowledge(st store.Store, user, id, by string) (store.Approval, bool, error) {
	var a store.Approval
	var changed bool
	err := st.InTx(bg(), func(tx store.Tx) error {
		var err error
		a, changed, err = tx.AcknowledgeApproval(user, id, by)
		return err
	})
	return a, changed, err
}

// settledApproval inserts an approval, approves it and moves it through
// executing to the state with the result (to executing only, if to is
// executing).
func settledApproval(t *testing.T, st store.Store, clk *Clock, c store.Claim, key string, to store.ApprovalState, result string) store.Approval {
	t.Helper()
	a := putApproval(t, st, c, approvalFor(c, clk, key))
	if _, err := decideApproval(st, c.Job.UserID, a.ID, store.ApprovalApproved, a.ArgsHash); err != nil {
		t.Fatal(err)
	}
	if _, err := transition(st, c.Job.UserID, a.ID, store.ApprovalApproved, store.ApprovalExecuting, ""); err != nil {
		t.Fatal(err)
	}
	if to != store.ApprovalExecuting {
		if _, err := transition(st, c.Job.UserID, a.ID, store.ApprovalExecuting, to, result); err != nil {
			t.Fatal(err)
		}
	}
	return getApproval(t, st, c.Job.UserID, a.ID)
}

func listVerify(t *testing.T, st store.Store, user string, f store.ApprovalFilter) []string {
	t.Helper()
	ids, to := scanVerify(t, st, user, f, 0)
	if to != nil {
		t.Fatalf("the scan stopped early at %+v", to)
	}
	return ids
}

func scanVerify(t *testing.T, st store.Store, user string, f store.ApprovalFilter, maxScan int) ([]string, *store.PageKey) {
	t.Helper()
	var ids []string
	var to *store.PageKey
	if err := st.View(bg(), func(tx store.Tx) error {
		list, k, err := tx.ListApprovalsToVerify(user, f, maxScan)
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		to = k
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids, to
}

func runAcknowledge(t *testing.T, f Factory) {
	t.Run("acknowledging records who and when and keeps state and result", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := settledApproval(t, st, clk, c, "1", store.ApprovalUnknown, `{"outcome":"unknown"}`)
		if a.AcknowledgedAt != nil || a.AcknowledgedBy != "" || !a.NeedsVerification() {
			t.Fatalf("fresh = %+v", a)
		}
		clk.Advance(time.Minute)
		got, changed, err := acknowledge(st, "u1", a.ID, "u1")
		if err != nil || !changed || got.AcknowledgedAt == nil || !got.AcknowledgedAt.Equal(clk.Now()) || got.AcknowledgedBy != "u1" {
			t.Fatalf("acknowledge = %+v %v %v", got, changed, err)
		}
		stored := getApproval(t, st, "u1", a.ID)
		if stored.State != store.ApprovalUnknown || string(stored.Result) != `{"outcome":"unknown"}` ||
			stored.AcknowledgedAt == nil || !stored.AcknowledgedAt.Equal(clk.Now()) || stored.AcknowledgedBy != "u1" ||
			stored.NeedsVerification() || !stored.OutcomeUncertain() {
			t.Fatalf("stored = %+v", stored)
		}
		// a second acknowledgement changes nothing and reports the first
		clk.Advance(time.Minute)
		again, changed, err := acknowledge(st, "u1", a.ID, "someone")
		if err != nil || changed || again.AcknowledgedBy != "u1" || !again.AcknowledgedAt.Equal(*stored.AcknowledgedAt) {
			t.Fatalf("again = %+v %v %v", again, changed, err)
		}
		// the state machine is untouched: nothing leaves a terminal state
		if _, err := transition(st, "u1", a.ID, store.ApprovalUnknown, store.ApprovalExecuting, ""); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("leaving unknown: %v", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalUnknown || got.AcknowledgedAt == nil {
			t.Fatalf("after = %+v", got)
		}
	})

	t.Run("an unreadable stored result can be acknowledged", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := settledApproval(t, st, clk, c, "1", store.ApprovalExecuted, `["strange"]`)
		got, changed, err := acknowledge(st, "u1", a.ID, "u1")
		if err != nil || !changed || got.State != store.ApprovalExecuted || got.NeedsVerification() {
			t.Fatalf("acknowledge = %+v %v %v", got, changed, err)
		}
	})

	t.Run("only an uncertain settled outcome can be acknowledged", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		pending := putApproval(t, st, c, approvalFor(c, clk, "p"))
		approved := putApproval(t, st, c, approvalFor(c, clk, "a"))
		if _, err := decideApproval(st, "u1", approved.ID, store.ApprovalApproved, approved.ArgsHash); err != nil {
			t.Fatal(err)
		}
		rejected := putApproval(t, st, c, approvalFor(c, clk, "r"))
		if _, err := decideApproval(st, "u1", rejected.ID, store.ApprovalRejected, rejected.ArgsHash); err != nil {
			t.Fatal(err)
		}
		executing := settledApproval(t, st, clk, c, "x", store.ApprovalExecuting, "")
		done := settledApproval(t, st, clk, c, "d", store.ApprovalExecuted, `{"outcome":"done"}`)
		failed := settledApproval(t, st, clk, c, "f", store.ApprovalFailed, `{"outcome":"failed","reason":"no"}`)
		for _, a := range []store.Approval{pending, approved, rejected, executing, done, failed} {
			if _, _, err := acknowledge(st, "u1", a.ID, "u1"); !errors.Is(err, store.ErrConflict) {
				t.Errorf("%s: err = %v, want ErrConflict", a.CallID, err)
			}
			if got := getApproval(t, st, "u1", a.ID); got.AcknowledgedAt != nil || got.AcknowledgedBy != "" {
				t.Errorf("%s: acknowledged = %+v", a.CallID, got)
			}
		}
		// a reconcile that settles the executing one as unknown makes it
		// acknowledgeable; a settled one never moves again
		if _, err := transition(st, "u1", executing.ID, store.ApprovalExecuting, store.ApprovalUnknown, `{"outcome":"unknown"}`); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := acknowledge(st, "u1", executing.ID, "u1"); err != nil || !changed {
			t.Fatalf("after the reconcile: %v %v", changed, err)
		}
	})

	t.Run("acknowledging is scoped and validated", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		mustUser(t, st, "u2")
		a := settledApproval(t, st, clk, c, "1", store.ApprovalUnknown, "")
		if _, _, err := acknowledge(st, "u2", a.ID, "u2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user: %v", err)
		}
		if _, _, err := acknowledge(st, "u1", "nope", "u1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown id: %v", err)
		}
		if _, _, err := acknowledge(st, "u1", a.ID, ""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("empty by: %v", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.AcknowledgedAt != nil {
			t.Errorf("acknowledged = %+v", got)
		}
	})

	t.Run("the verification filter lists only unacknowledged uncertain outcomes", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		u1 := settledApproval(t, st, clk, c, "1", store.ApprovalUnknown, `{"outcome":"unknown"}`)
		clk.Advance(time.Second)
		settledApproval(t, st, clk, c, "2", store.ApprovalExecuted, `{"outcome":"done"}`)
		clk.Advance(time.Second)
		bad := settledApproval(t, st, clk, c, "3", store.ApprovalExecuted, `{"outcome":"done","result":{"summary":5}}`)
		clk.Advance(time.Second)
		settledApproval(t, st, clk, c, "4", store.ApprovalFailed, `{"outcome":"failed"}`)
		clk.Advance(time.Second)
		u2 := settledApproval(t, st, clk, c, "5", store.ApprovalUnknown, "")
		clk.Advance(time.Second)
		acked := settledApproval(t, st, clk, c, "6", store.ApprovalUnknown, "")
		if _, _, err := acknowledge(st, "u1", acked.ID, "u1"); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
		putApproval(t, st, c, approvalFor(c, clk, "7")) // pending

		want := []string{u2.ID, bad.ID, u1.ID}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{}); !equalIDs(got, want) {
			t.Errorf("all = %v, want %v", got, want)
		}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{Limit: 2}); !equalIDs(got, want[:2]) {
			t.Errorf("limit 2 = %v", got)
		}
		k := store.PageKey{At: getApproval(t, st, "u1", bad.ID).RequestedAt, ID: bad.ID}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{Before: &k}); !equalIDs(got, want[2:]) {
			t.Errorf("after the cursor = %v", got)
		}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{States: []store.ApprovalState{store.ApprovalExecuted}}); !equalIDs(got, []string{bad.ID}) {
			t.Errorf("executed only = %v", got)
		}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{MaxBytes: 1}); !equalIDs(got, want[:1]) {
			t.Errorf("byte budget = %v", got)
		}
		if got := listVerify(t, st, "u1", store.ApprovalFilter{JobID: "other"}); len(got) != 0 {
			t.Errorf("another job = %v", got)
		}
		mustUser(t, st, "u2")
		if got := listVerify(t, st, "u2", store.ApprovalFilter{}); len(got) != 0 {
			t.Errorf("another user = %v", got)
		}
	})

	t.Run("the verification scan examines at most maxScan candidates", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		old := settledApproval(t, st, clk, c, "old", store.ApprovalUnknown, "")
		var certain []store.Approval
		for i := 0; i < 5; i++ {
			clk.Advance(time.Second)
			certain = append(certain, settledApproval(t, st, clk, c, string(rune('a'+i)), store.ApprovalExecuted, `{"outcome":"done"}`))
		}
		clk.Advance(time.Second)
		putApproval(t, st, c, approvalFor(c, clk, "pending")) // no candidate: not counted
		clk.Advance(time.Second)
		newest := settledApproval(t, st, clk, c, "new", store.ApprovalUnknown, "")

		// newest first: new, e, d, c (the cap), then b, a, old
		ids, to := scanVerify(t, st, "u1", store.ApprovalFilter{}, 4)
		if !equalIDs(ids, []string{newest.ID}) || to == nil || to.ID != certain[2].ID ||
			!to.At.Equal(getApproval(t, st, "u1", certain[2].ID).RequestedAt) {
			t.Fatalf("first scan = %v, stopped at %+v", ids, to)
		}
		ids, to = scanVerify(t, st, "u1", store.ApprovalFilter{Before: to}, 2)
		if len(ids) != 0 || to == nil || to.ID != certain[0].ID {
			t.Fatalf("second scan = %v, stopped at %+v", ids, to)
		}
		ids, to = scanVerify(t, st, "u1", store.ApprovalFilter{Before: to}, 2)
		if !equalIDs(ids, []string{old.ID}) || to != nil {
			t.Fatalf("last scan = %v, stopped at %+v", ids, to)
		}
		// a scan that ends on its limit does not report a stop
		if ids, to := scanVerify(t, st, "u1", store.ApprovalFilter{Limit: 1}, 1); !equalIDs(ids, []string{newest.ID}) || to != nil {
			t.Fatalf("limit = %v, %+v", ids, to)
		}
		// the default cap covers this history
		if ids, to := scanVerify(t, st, "u1", store.ApprovalFilter{}, 0); !equalIDs(ids, []string{newest.ID, old.ID}) || to != nil {
			t.Fatalf("default = %v, %+v", ids, to)
		}
	})
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
