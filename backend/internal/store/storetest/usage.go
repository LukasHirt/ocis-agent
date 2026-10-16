package storetest

import (
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Usage", runUsage) }

func appendStep(t *testing.T, st store.Store, c store.Claim, kind string, in, out int64) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error {
		_, err := tx.AppendStep(store.JobStep{JobID: c.Job.ID, Kind: kind, Status: "ok", TokensIn: in, TokensOut: out}, c.Fence)
		return err
	})
}

func sumTokens(t *testing.T, st store.Store, user string, since time.Time) (int64, int64) {
	t.Helper()
	var in, out int64
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		in, out, err = tx.SumJobTokens(user, since)
		return err
	})
	if err != nil {
		t.Fatalf("SumJobTokens(%s): %v", user, err)
	}
	return in, out
}

func runUsage(t *testing.T, f Factory) {
	t.Run("sum counts steps by their own time, both kinds, finished jobs, one user", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		start := clk.Now()

		addJob(t, st, "u1", "first")
		c1 := mustClaim(t, st, "w")
		appendStep(t, st, c1, "llm", 10, 5) // at start
		clk.Advance(time.Hour)
		appendStep(t, st, c1, "tool", 0, 0)
		appendStep(t, st, c1, "llm", 7, 3) // at start+1h
		if !release(t, st, c1, store.JobSucceeded) {
			t.Fatal("release not applied")
		}

		addJob(t, st, "u1", "second")
		addJob(t, st, "u2", "other")
		c2 := mustClaim(t, st, "w")
		c3 := mustClaim(t, st, "w2")
		clk.Advance(time.Hour)
		appendStep(t, st, c2, "llm", 100, 50) // at start+2h, running job
		appendStep(t, st, c3, "llm", 1000, 500)

		if in, out := sumTokens(t, st, "u1", time.Time{}); in != 117 || out != 58 {
			t.Errorf("u1 from the beginning = %d/%d, want 117/58", in, out)
		}
		if in, out := sumTokens(t, st, "u1", start.Add(time.Hour)); in != 107 || out != 53 {
			t.Errorf("u1 since +1h (inclusive) = %d/%d, want 107/53", in, out)
		}
		if in, out := sumTokens(t, st, "u1", start.Add(2*time.Hour+time.Second)); in != 0 || out != 0 {
			t.Errorf("u1 after every step = %d/%d, want 0/0", in, out)
		}
		if in, out := sumTokens(t, st, "u2", time.Time{}); in != 1000 || out != 500 {
			t.Errorf("u2 = %d/%d, want 1000/500", in, out)
		}
	})

	t.Run("sum of an unknown user or a user without steps is zero", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "idle")
		if in, out := sumTokens(t, st, "ghost", time.Time{}); in != 0 || out != 0 {
			t.Errorf("ghost = %d/%d", in, out)
		}
		if in, out := sumTokens(t, st, "u1", time.Time{}); in != 0 || out != 0 {
			t.Errorf("u1 = %d/%d", in, out)
		}
	})

	t.Run("claim skips a paused user until the pause is cleared", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j1 := addJob(t, st, "u1", "paused user's job")
		j2 := addJob(t, st, "u2", "other user's job")
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.PausePolicy("u1", store.PauseUser)
			return err
		})
		c := mustClaim(t, st, "w")
		if c.Job.ID != j2.ID {
			t.Fatalf("claimed %s, want the other user's job %s", c.Job.ID, j2.ID)
		}
		release(t, st, c, store.JobSucceeded)
		if c2, ok := claimNext(t, st, "w"); ok {
			t.Fatalf("claimed %s of a paused user", c2.Job.ID)
		}
		if got := getJob(t, st, "u1", j1.ID); got.State != store.JobQueued {
			t.Errorf("paused user's job state = %s, want queued", got.State)
		}
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.ClearPause("u1", store.PauseUser)
			return err
		})
		if c3 := mustClaim(t, st, "w"); c3.Job.ID != j1.ID {
			t.Errorf("after ClearPause claimed %s, want %s", c3.Job.ID, j1.ID)
		}
	})

	t.Run("pausing does not touch a running job", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "running")
		c := mustClaim(t, st, "w")
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.PausePolicy("u1", store.PauseUser)
			return err
		})
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobRunning {
			t.Errorf("state = %s, want running", got.State)
		}
		appendStep(t, st, c, "llm", 1, 1)
	})
}
