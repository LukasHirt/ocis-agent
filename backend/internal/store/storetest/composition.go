package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Composition", runComposition) }

// waitForWithExpiry is waitFor with an explicit expiry per approval.
func waitForWithExpiry(t *testing.T, st store.Store, clk *Clock, c store.Claim, ttls ...time.Duration) []store.Approval {
	t.Helper()
	var out []store.Approval
	mustTx(t, st, func(tx store.Tx) error {
		for i, ttl := range ttls {
			a := approvalFor(c, clk, string(rune('a'+i)))
			a.ExpiresAt = clk.Now().Add(ttl)
			got, err := tx.InsertApproval(a, c.Fence)
			if err != nil {
				return err
			}
			out = append(out, got)
		}
		_, err := tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		return err
	})
	return out
}

func runComposition(t *testing.T, f Factory) {
	t.Run("expire and resume in one transaction", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		waitForWithExpiry(t, st, clk, c, time.Hour, 10*time.Hour)

		sweep := func() (refs []store.ApprovalRef, resumed []bool) {
			mustTx(t, st, func(tx store.Tx) error {
				var err error
				if refs, err = tx.ExpireApprovals(); err != nil {
					return err
				}
				for _, r := range refs {
					ok, err := tx.ResumeJobIfSettled(r.UserID, r.JobID)
					if err != nil {
						return err
					}
					resumed = append(resumed, ok)
				}
				return nil
			})
			return refs, resumed
		}

		clk.Advance(2 * time.Hour)
		refs, resumed := sweep()
		if len(refs) != 1 || len(resumed) != 1 || resumed[0] {
			t.Fatalf("first sweep: refs = %+v, resumed = %v, want one ref and no resume", refs, resumed)
		}
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobWaitingApproval {
			t.Fatalf("job = %s, want waiting_approval while one approval is pending", got.State)
		}
		clk.Advance(10 * time.Hour)
		refs, resumed = sweep()
		if len(refs) != 1 || len(resumed) != 1 || !resumed[0] {
			t.Fatalf("second sweep: refs = %+v, resumed = %v, want one ref and a resume", refs, resumed)
		}
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobQueued || !got.NotBefore.Equal(clk.Now()) {
			t.Fatalf("job = %+v, want queued and claimable now", got)
		}
	})

	t.Run("decide and resume in one transaction", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		as := waitFor(t, st, clk, c, 2)

		decide := func(id string, d store.ApprovalState) (resumed bool) {
			mustTx(t, st, func(tx store.Tx) error {
				if _, err := tx.DecideApproval("u1", id, d, "alice", "sha256:abc"); err != nil {
					return err
				}
				var err error
				resumed, err = tx.ResumeJobIfSettled("u1", c.Job.ID)
				return err
			})
			return resumed
		}
		if decide(as[0].ID, store.ApprovalApproved) {
			t.Fatal("resumed with one approval still pending")
		}
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobWaitingApproval {
			t.Fatalf("job = %s, want waiting_approval", got.State)
		}
		if !decide(as[1].ID, store.ApprovalRejected) {
			t.Fatal("did not resume after the last decision")
		}
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobQueued {
			t.Fatalf("job = %s, want queued", got.State)
		}
	})

	t.Run("a failed decision rolls back the resume", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		as := waitFor(t, st, clk, c, 1)
		err := st.InTx(bg(), func(tx store.Tx) error {
			if _, err := tx.DecideApproval("u1", as[0].ID, store.ApprovalApproved, "alice", "sha256:abc"); err != nil {
				return err
			}
			if _, err := tx.ResumeJobIfSettled("u1", c.Job.ID); err != nil {
				return err
			}
			return errInjected
		})
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v", err)
		}
		if got := getApproval(t, st, "u1", as[0].ID); got.State != store.ApprovalPending {
			t.Errorf("approval = %s, want pending", got.State)
		}
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobWaitingApproval {
			t.Errorf("job = %s, want waiting_approval", got.State)
		}
	})

	t.Run("resume with a mix of expired and decided approvals", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		as := waitForWithExpiry(t, st, clk, c, time.Hour, 10*time.Hour, 10*time.Hour)
		if _, err := decideApproval(st, "u1", as[1].ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}
		clk.Advance(2 * time.Hour)
		if refs := expireApprovals(t, st); len(refs) != 1 || refs[0].ID != as[0].ID {
			t.Fatalf("refs = %+v", refs)
		}
		if resume(t, st, "u1", c.Job.ID) {
			t.Fatal("resumed with the third approval still pending")
		}
		if _, err := decideApproval(st, "u1", as[2].ID, store.ApprovalRejected, "sha256:abc"); err != nil {
			t.Fatal(err)
		}
		if !resume(t, st, "u1", c.Job.ID) {
			t.Fatal("did not resume with one expired, one approved and one rejected")
		}
	})

	t.Run("another user cannot touch the approval or the job", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		mustUser(t, st, "u2")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}

		if _, err := transition(st, "u2", a.ID, store.ApprovalApproved, store.ApprovalExecuting, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("u2 TransitionApproval: err = %v, want ErrNotFound", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalApproved {
			t.Errorf("approval = %s, want it unchanged", got.State)
		}

		foreign := approvalFor(c, clk, "2")
		foreign.UserID = "u2"
		err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.InsertApproval(foreign, c.Fence); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("u2 InsertApproval on u1's job: err = %v, want ErrNotFound", err)
		}
		if n := len(listApprovals(t, st, "u1")); n != 1 {
			t.Errorf("u1 has %d approvals, want 1", n)
		}
	})
}

func listApprovals(t *testing.T, st store.Store, user string) []store.Approval {
	t.Helper()
	var out []store.Approval
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListApprovals(user, store.ApprovalFilter{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
