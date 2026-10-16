package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("SettleEnded", runSettleEnded) }

// pendingFor claims the user's next job, inserts n pending approvals (keys
// prefixed with tag) and releases the job into waiting_approval.
func pendingFor(t *testing.T, st store.Store, clk *Clock, user, tag string, n int) (store.Claim, []store.Approval) {
	t.Helper()
	addJob(t, st, user, "job "+tag)
	c := mustClaim(t, st, "w-"+tag)
	var out []store.Approval
	mustTx(t, st, func(tx store.Tx) error {
		for i := 0; i < n; i++ {
			a, err := tx.InsertApproval(approvalFor(c, clk, fmt.Sprintf("%s-%d", tag, i)), c.Fence)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		_, err := tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		return err
	})
	return c, out
}

func cancelJob(t *testing.T, st store.Store, user, job string) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob(user, job) })
}

func settleEnded(t *testing.T, st store.Store, limit int) []store.ApprovalRef {
	t.Helper()
	var refs []store.ApprovalRef
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		refs, err = tx.ExpireEndedJobApprovals(limit)
		return err
	})
	return refs
}

func refIDs(refs []store.ApprovalRef) []string {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

func runSettleEnded(t *testing.T, f Factory) {
	t.Run("a cancelled job's pending approval and a failed job's approved approval are expired", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")

		cancelled, pa := pendingFor(t, st, clk, "u1", "c", 1)
		cancelJob(t, st, "u1", cancelled.Job.ID)

		clk.Advance(time.Second)
		failed, aa := pendingFor(t, st, clk, "u2", "f", 1)
		if _, err := decideApproval(st, "u2", aa[0].ID, store.ApprovalApproved, aa[0].ArgsHash); err != nil {
			t.Fatal(err)
		}
		if !resume(t, st, "u2", failed.Job.ID) {
			t.Fatal("job not resumed")
		}
		decided := getApproval(t, st, "u2", aa[0].ID)
		c2 := mustClaim(t, st, "w")
		if !release(t, st, c2, store.JobFailed) {
			t.Fatal("release not applied")
		}

		clk.Advance(time.Minute)
		refs := settleEnded(t, st, 0)
		want := []store.ApprovalRef{
			{ID: pa[0].ID, UserID: "u1", JobID: cancelled.Job.ID},
			{ID: aa[0].ID, UserID: "u2", JobID: failed.Job.ID},
		}
		if len(refs) != 2 || refs[0] != want[0] || refs[1] != want[1] {
			t.Fatalf("refs = %+v, want %+v (oldest first)", refs, want)
		}
		if got := getApproval(t, st, "u1", pa[0].ID); got.State != store.ApprovalExpired || got.DecidedBy != "" || got.DecidedAt != nil {
			t.Fatalf("pending approval = %+v, want expired and undecided", got)
		}
		got := getApproval(t, st, "u2", aa[0].ID)
		if got.State != store.ApprovalExpired || got.DecidedBy != decided.DecidedBy || got.DecidedAt == nil ||
			!got.DecidedAt.Equal(*decided.DecidedAt) || got.ExecutedAt != nil {
			t.Fatalf("approved approval = %+v, want expired with the decision kept (%+v)", got, decided)
		}
		if again := settleEnded(t, st, 0); len(again) != 0 {
			t.Fatalf("second call settled %+v, want nothing", again)
		}
	})

	t.Run("a succeeded job's approved approval is expired", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		c, as := pendingFor(t, st, clk, "u1", "s", 1)
		if _, err := decideApproval(st, "u1", as[0].ID, store.ApprovalApproved, as[0].ArgsHash); err != nil {
			t.Fatal(err)
		}
		resume(t, st, "u1", c.Job.ID)
		if !release(t, st, mustClaim(t, st, "w"), store.JobSucceeded) {
			t.Fatal("release not applied")
		}
		if refs := settleEnded(t, st, 0); len(refs) != 1 || refs[0].ID != as[0].ID {
			t.Fatalf("refs = %+v", refs)
		}
	})

	t.Run("approvals of jobs that have not ended are never touched", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		mustUser(t, st, "u3")

		// waiting_approval with a pending approval
		_, waiting := pendingFor(t, st, clk, "u1", "w", 1)
		queued, qa := pendingFor(t, st, clk, "u2", "q", 1)
		running, ra := pendingFor(t, st, clk, "u3", "r", 1)
		// running with an approved approval: the only runnable job is u3's
		if _, err := decideApproval(st, "u3", ra[0].ID, store.ApprovalApproved, ra[0].ArgsHash); err != nil {
			t.Fatal(err)
		}
		resume(t, st, "u3", running.Job.ID)
		if c := mustClaim(t, st, "w"); c.Job.ID != running.Job.ID {
			t.Fatalf("claimed %s, want u3's job", c.Job.ID)
		}
		// queued with an approved approval
		if _, err := decideApproval(st, "u2", qa[0].ID, store.ApprovalApproved, qa[0].ArgsHash); err != nil {
			t.Fatal(err)
		}
		resume(t, st, "u2", queued.Job.ID)
		if getJob(t, st, "u1", waiting[0].JobID).State != store.JobWaitingApproval ||
			getJob(t, st, "u2", queued.Job.ID).State != store.JobQueued ||
			getJob(t, st, "u3", running.Job.ID).State != store.JobRunning {
			t.Fatal("setup: unexpected job states")
		}

		clk.Advance(time.Hour)
		if refs := settleEnded(t, st, 0); len(refs) != 0 {
			t.Fatalf("settled %+v, want nothing", refs)
		}
		if getApproval(t, st, "u1", waiting[0].ID).State != store.ApprovalPending ||
			getApproval(t, st, "u2", qa[0].ID).State != store.ApprovalApproved ||
			getApproval(t, st, "u3", ra[0].ID).State != store.ApprovalApproved {
			t.Fatal("an approval of a live job changed")
		}
	})

	t.Run("executing and already settled approvals of ended jobs are never touched", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		c, as := pendingFor(t, st, clk, "u1", "x", 6)
		// executing, executed, failed, unknown (all via approved), rejected, expired
		for _, a := range as[:4] {
			if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, a.ArgsHash); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := decideApproval(st, "u1", as[4].ID, store.ApprovalRejected, as[4].ArgsHash); err != nil {
			t.Fatal(err)
		}
		clk.Advance(approvalTTL)
		if refs := expireApprovals(t, st); len(refs) != 1 || refs[0].ID != as[5].ID {
			t.Fatalf("expired %+v", refs)
		}
		resume(t, st, "u1", c.Job.ID)
		mustClaim(t, st, "w")
		for i, to := range []store.ApprovalState{store.ApprovalExecuting, store.ApprovalExecuted, store.ApprovalFailed, store.ApprovalUnknown} {
			if _, err := transition(st, "u1", as[i].ID, store.ApprovalApproved, store.ApprovalExecuting, ""); err != nil {
				t.Fatal(err)
			}
			if to != store.ApprovalExecuting {
				if _, err := transition(st, "u1", as[i].ID, store.ApprovalExecuting, to, `{"ok":true}`); err != nil {
					t.Fatal(err)
				}
			}
		}
		cancelJob(t, st, "u1", c.Job.ID)

		before := map[string]store.Approval{}
		for _, a := range as {
			before[a.ID] = getApproval(t, st, "u1", a.ID)
		}
		if refs := settleEnded(t, st, 0); len(refs) != 0 {
			t.Fatalf("settled %+v, want nothing", refs)
		}
		for _, a := range as {
			got := getApproval(t, st, "u1", a.ID)
			if got.State != before[a.ID].State || got.DecidedBy != before[a.ID].DecidedBy || string(got.Result) != string(before[a.ID].Result) {
				t.Fatalf("approval %s changed: %+v -> %+v", a.ID, before[a.ID], got)
			}
		}
		if s := getApproval(t, st, "u1", as[0].ID).State; s != store.ApprovalExecuting {
			t.Fatalf("executing approval = %s", s)
		}
	})

	t.Run("the limit takes the oldest first and the rest on the next call", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		var all []store.Approval
		for i, u := range []string{"u2", "u1", "u2"} {
			c, as := pendingFor(t, st, clk, u, fmt.Sprintf("l%d", i), 1)
			cancelJob(t, st, u, c.Job.ID)
			all = append(all, as...)
			clk.Advance(time.Second)
		}
		first := settleEnded(t, st, 2)
		if ids := refIDs(first); len(ids) != 2 || ids[0] != all[0].ID || ids[1] != all[1].ID {
			t.Fatalf("first batch = %v, want the two oldest %s %s", ids, all[0].ID, all[1].ID)
		}
		if getApproval(t, st, "u2", all[2].ID).State != store.ApprovalPending {
			t.Fatal("the newest approval must wait for the next call")
		}
		second := settleEnded(t, st, 2)
		if ids := refIDs(second); len(ids) != 1 || ids[0] != all[2].ID {
			t.Fatalf("second batch = %v, want %s", ids, all[2].ID)
		}
	})

	t.Run("a limit of zero or less means 100, the maximum is 1000", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		c, _ := pendingFor(t, st, clk, "u1", "m", 1201)
		cancelJob(t, st, "u1", c.Job.ID)
		if refs := settleEnded(t, st, 0); len(refs) != 100 {
			t.Fatalf("limit 0 settled %d, want 100", len(refs))
		}
		if refs := settleEnded(t, st, -1); len(refs) != 100 {
			t.Fatalf("limit -1 settled %d, want 100", len(refs))
		}
		if refs := settleEnded(t, st, 5000); len(refs) != 1000 {
			t.Fatalf("limit 5000 settled %d, want 1000", len(refs))
		}
		if refs := settleEnded(t, st, 5000); len(refs) != 1 {
			t.Fatalf("the last call settled %d, want the remaining 1", len(refs))
		}
	})
}
