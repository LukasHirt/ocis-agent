package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Leases", runLeases) }

func assertLease(st store.Store, user, job string, fence int64) error {
	return st.InTx(bg(), func(tx store.Tx) error { return tx.AssertLease(user, job, fence) })
}

func requeue(t *testing.T, st store.Store, c store.Claim, notBefore time.Time) bool {
	t.Helper()
	var applied bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		applied, err = tx.RequeueJob(c, notBefore, nil)
		return err
	})
	return applied
}

func runLeases(t *testing.T, f Factory) {
	t.Run("AssertLease accepts the current claim and rejects the rest", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")

		if err := assertLease(st, "u1", j.ID, c.Fence); err != nil {
			t.Fatalf("current claim rejected: %v", err)
		}
		if err := assertLease(st, "u1", j.ID, c.Fence+1); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("wrong fence: err = %v, want ErrFenceLost", err)
		}
		if err := assertLease(st, "u2", j.ID, c.Fence); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's job: err = %v, want ErrNotFound", err)
		}
		if err := assertLease(st, "u1", "nope", c.Fence); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown job: err = %v, want ErrNotFound", err)
		}
		release(t, st, c, store.JobWaitingApproval)
		if err := assertLease(st, "u1", j.ID, c.Fence); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("after release: err = %v, want ErrFenceLost", err)
		}
	})

	t.Run("AssertLease reports a terminal job as a conflict", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		if err := assertLease(st, "u1", j.ID, c.Fence); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("a stale claim fails AssertLease after a takeover", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j1 := addJob(t, st, "u1", "j1")
		clk.Advance(time.Second)
		addJob(t, st, "u1", "j2")
		c1 := mustClaim(t, st, "w1")
		clk.Advance(3 * time.Minute)
		c2 := mustClaim(t, st, "w2")
		if err := assertLease(st, "u1", j1.ID, c1.Fence); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale worker: err = %v, want ErrFenceLost", err)
		}
		if err := assertLease(st, "u1", c2.Job.ID, c2.Fence); err != nil {
			t.Errorf("current worker: %v", err)
		}
	})

	t.Run("runs count every claim while attempts count only crashes", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")

		c1 := mustClaim(t, st, "w1")
		if c1.Job.Runs != 1 || c1.Job.Attempt != 1 {
			t.Fatalf("first claim: runs %d attempt %d", c1.Job.Runs, c1.Job.Attempt)
		}
		if !requeue(t, st, c1, clk.Now().Add(time.Minute)) {
			t.Fatal("requeue was not applied")
		}
		got := getJob(t, st, "u1", j.ID)
		if got.State != store.JobQueued || got.Runs != 1 || got.Attempt != 0 || !got.NotBefore.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("after requeue: %+v", got)
		}
		if _, ok := claimNext(t, st, "w2"); ok {
			t.Fatal("claimed before the requeue delay passed")
		}
		clk.Advance(time.Minute)
		c2 := mustClaim(t, st, "w2")
		if c2.Job.Runs != 2 || c2.Job.Attempt != 1 {
			t.Fatalf("second claim: runs %d attempt %d", c2.Job.Runs, c2.Job.Attempt)
		}

		// a crash: the reaper requeues, the attempt stays, the next claim is run 3 / attempt 2
		clk.Advance(3 * time.Minute)
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.ReapExpired(5, backoffMinutes)
			return err
		})
		clk.Advance(time.Minute)
		c3 := mustClaim(t, st, "w3")
		if c3.Job.Runs != 3 || c3.Job.Attempt != 2 {
			t.Fatalf("third claim: runs %d attempt %d", c3.Job.Runs, c3.Job.Attempt)
		}
	})

	t.Run("a zero requeue time makes the job claimable at once", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		if !requeue(t, st, c, time.Time{}) {
			t.Fatal("requeue was not applied")
		}
		if c2 := mustClaim(t, st, "w2"); c2.Job.Runs != 2 {
			t.Fatalf("runs = %d", c2.Job.Runs)
		}
	})

	t.Run("requeue after cancel is not applied and keeps the job cancelled", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		if requeue(t, st, c, clk.Now().Add(time.Hour)) {
			t.Fatal("requeue was applied to a cancelled job")
		}
		if got := getJob(t, st, "u1", j.ID); got.State != store.JobCancelled {
			t.Fatalf("state = %s", got.State)
		}
	})

	t.Run("requeue needs the current fence", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		stale := c
		stale.Fence++
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.RequeueJob(stale, time.Time{}, nil)
			return err
		})
		if !errors.Is(err, store.ErrFenceLost) {
			t.Fatalf("err = %v, want ErrFenceLost", err)
		}
	})
}
