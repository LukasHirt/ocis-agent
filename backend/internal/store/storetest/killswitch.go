package storetest

import (
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("KillSwitch", runKillSwitch) }

func setKillSwitch(t *testing.T, st store.Store, v string) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, v) })
}

func runKillSwitch(t *testing.T, f Factory) {
	t.Run("nothing is claimed while the kill switch is on", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j1 := addJob(t, st, "u1", "first")
		j2 := addJob(t, st, "u2", "second")
		setKillSwitch(t, st, store.KillSwitchOnValue)
		if c, ok := claimNext(t, st, "w1"); ok {
			t.Fatalf("claimed %s with the kill switch on", c.Job.ID)
		}
		for _, j := range []store.Job{j1, j2} {
			got := getJob(t, st, j.UserID, j.ID)
			if got.State != store.JobQueued || got.Runs != 0 || got.Attempt != 0 {
				t.Errorf("job %s = %s runs %d attempt %d, want queued and untouched", j.Goal, got.State, got.Runs, got.Attempt)
			}
		}

		setKillSwitch(t, st, "0")
		if c := mustClaim(t, st, "w1"); c.Job.ID != j1.ID || c.Job.Runs != 1 {
			t.Errorf("first claim after off = %s runs %d, want the oldest job", c.Job.Goal, c.Job.Runs)
		}
		if c := mustClaim(t, st, "w2"); c.Job.ID != j2.ID {
			t.Errorf("second claim after off = %s, want the next job", c.Job.Goal)
		}
	})

	t.Run("only the value 1 holds the switch on", func(t *testing.T) {
		for _, v := range []string{"0", "", "true", "on", " 1", "11"} {
			st, _ := open(t, f)
			mustUser(t, st, "u1")
			addJob(t, st, "u1", "j")
			setKillSwitch(t, st, v)
			if _, ok := claimNext(t, st, "w1"); !ok {
				t.Errorf("value %q: nothing claimed, want the job", v)
			}
		}
	})

	t.Run("a running job is not touched by the switch", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		setKillSwitch(t, st, store.KillSwitchOnValue)
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobRunning {
			t.Errorf("running job = %s", got.State)
		}
		if err := assertLease(st, "u1", c.Job.ID, c.Fence); err != nil {
			t.Errorf("lease: %v", err)
		}
	})
}
