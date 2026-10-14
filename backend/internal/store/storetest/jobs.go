package storetest

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Jobs", runJobs) }

const leaseTTL = 2 * time.Minute

func addJob(t *testing.T, st store.Store, user, goal string) store.Job {
	t.Helper()
	var j store.Job
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		j, err = tx.InsertJob(store.Job{UserID: user, TriggerKind: "chat", Goal: goal})
		return err
	})
	return j
}

func claimNext(t *testing.T, st store.Store, worker string) (store.Claim, bool) {
	t.Helper()
	var c store.Claim
	var ok bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		c, ok, err = tx.ClaimNextJob(worker, leaseTTL)
		return err
	})
	return c, ok
}

func mustClaim(t *testing.T, st store.Store, worker string) store.Claim {
	t.Helper()
	c, ok := claimNext(t, st, worker)
	if !ok {
		t.Fatalf("%s: nothing to claim", worker)
	}
	return c
}

func getJob(t *testing.T, st store.Store, user, id string) store.Job {
	t.Helper()
	var j store.Job
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(user, id)
		return err
	})
	if err != nil {
		t.Fatalf("GetJob(%s): %v", id, err)
	}
	return j
}

func release(t *testing.T, st store.Store, c store.Claim, next store.JobState) bool {
	t.Helper()
	var applied bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		applied, err = tx.ReleaseJob(c, next, nil, "")
		return err
	})
	return applied
}

func saveCheckpoint(st store.Store, user, job string, fence int64) error {
	return st.InTx(bg(), func(tx store.Tx) error {
		return tx.SaveCheckpoint(user, job, json.RawMessage(`{"n":1}`), fence)
	})
}

func backoffMinutes(attempt int) time.Duration { return time.Duration(attempt) * time.Minute }

func runJobs(t *testing.T, f Factory) {
	t.Run("insert applies defaults and get round trips", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "tidy up")
		if j.ID == "" || j.State != store.JobQueued || j.Attempt != 0 || string(j.Checkpoint) != "{}" ||
			!j.NotBefore.Equal(clk.Now()) || !j.CreatedAt.Equal(clk.Now()) || !j.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("job = %+v", j)
		}
		got := getJob(t, st, "u1", j.ID)
		if got.Goal != "tidy up" || got.TriggerKind != "chat" || got.State != store.JobQueued {
			t.Errorf("GetJob = %+v", got)
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetJob("u2", j.ID)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's GetJob: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("insert validation", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		bad := []store.Job{
			{TriggerKind: "chat", Goal: "g"},
			{UserID: "u1", Goal: "g"},
			{UserID: "u1", TriggerKind: "chat"},
			{UserID: "u1", TriggerKind: "chat", Goal: "g", Checkpoint: json.RawMessage(`{broken`)},
		}
		for _, j := range bad {
			if err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.InsertJob(j); return err }); err == nil {
				t.Errorf("InsertJob(%+v) succeeded", j)
			}
		}
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.InsertJob(store.Job{UserID: "ghost", TriggerKind: "chat", Goal: "g"})
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("list filters and orders newest first", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		a := addJob(t, st, "u1", "a")
		clk.Advance(time.Second)
		b := addJob(t, st, "u1", "b")
		clk.Advance(time.Second)
		c := addJob(t, st, "u1", "c")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", b.ID) })

		list := func(f store.JobFilter) []store.Job {
			var out []store.Job
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				out, err = tx.ListJobs("u1", f)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if all := list(store.JobFilter{}); len(all) != 3 || all[0].ID != c.ID || all[1].ID != b.ID || all[2].ID != a.ID {
			t.Errorf("all = %+v", all)
		}
		if q := list(store.JobFilter{States: []store.JobState{store.JobQueued}}); len(q) != 2 || q[0].ID != c.ID || q[1].ID != a.ID {
			t.Errorf("queued = %+v", q)
		}
		if one := list(store.JobFilter{Limit: 1}); len(one) != 1 || one[0].ID != c.ID {
			t.Errorf("limit 1 = %+v", one)
		}
	})

	t.Run("claim order and one run per user", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j1 := addJob(t, st, "u1", "j1")
		clk.Advance(time.Second)
		j2 := addJob(t, st, "u2", "j2")
		clk.Advance(time.Second)
		j3 := addJob(t, st, "u1", "j3")

		c1 := mustClaim(t, st, "w1")
		if c1.Job.ID != j1.ID || c1.Fence != 1 || c1.Job.State != store.JobRunning || c1.Job.Attempt != 1 ||
			c1.Worker != "w1" || !c1.ExpiresAt.Equal(clk.Now().Add(leaseTTL)) {
			t.Fatalf("first claim = %+v", c1)
		}
		c2 := mustClaim(t, st, "w2")
		if c2.Job.ID != j2.ID || c2.Fence != 1 {
			t.Fatalf("second claim = %+v", c2)
		}
		if _, ok := claimNext(t, st, "w3"); ok {
			t.Fatal("j3 was claimed while u1 still holds a lease")
		}
		if !release(t, st, c1, store.JobSucceeded) {
			t.Fatal("release was not applied")
		}
		c3 := mustClaim(t, st, "w3")
		if c3.Job.ID != j3.ID || c3.Fence != 2 {
			t.Fatalf("third claim = %+v (fence must keep increasing)", c3)
		}
	})

	t.Run("a job with a future not_before is not claimed early", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.InsertJob(store.Job{UserID: "u1", TriggerKind: "schedule", Goal: "later", NotBefore: clk.Now().Add(time.Hour)})
			return err
		})
		if _, ok := claimNext(t, st, "w1"); ok {
			t.Fatal("claimed before not_before")
		}
		clk.Advance(time.Hour)
		if _, ok := claimNext(t, st, "w1"); !ok {
			t.Fatal("not claimed after not_before")
		}
	})

	t.Run("takeover after expiry fences the old worker", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j1 := addJob(t, st, "u1", "j1")
		clk.Advance(time.Second)
		j2 := addJob(t, st, "u1", "j2")

		c1 := mustClaim(t, st, "w1")
		clk.Advance(3 * time.Minute) // the lease expires
		c2 := mustClaim(t, st, "w2")
		if c2.Job.ID != j2.ID || c2.Fence != 2 {
			t.Fatalf("takeover claim = %+v", c2)
		}

		if err := saveCheckpoint(st, "u1", j1.ID, c1.Fence); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale SaveCheckpoint: err = %v, want ErrFenceLost", err)
		}
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.AppendStep(store.JobStep{JobID: j1.ID, Kind: "tool", Status: "ok"}, c1.Fence)
			return err
		})
		if !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale AppendStep: err = %v, want ErrFenceLost", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { _, err := tx.RenewLease(c1, leaseTTL); return err })
		if !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale RenewLease: err = %v, want ErrFenceLost", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { _, err := tx.ReleaseJob(c1, store.JobSucceeded, nil, ""); return err })
		if !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale ReleaseJob: err = %v, want ErrFenceLost", err)
		}
		if err := saveCheckpoint(st, "u1", j2.ID, c2.Fence); err != nil {
			t.Errorf("current worker's SaveCheckpoint: %v", err)
		}
	})

	t.Run("renew extends the lease", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		addJob(t, st, "u1", "j1")
		clk.Advance(time.Second)
		addJob(t, st, "u1", "j2")
		c1 := mustClaim(t, st, "w1")

		clk.Advance(90 * time.Second)
		var renewed store.Claim
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			renewed, err = tx.RenewLease(c1, leaseTTL)
			return err
		})
		if !renewed.ExpiresAt.Equal(clk.Now().Add(leaseTTL)) {
			t.Errorf("ExpiresAt = %v", renewed.ExpiresAt)
		}
		clk.Advance(90 * time.Second) // 180s after the claim: past the original ttl, within the renewed one
		if _, ok := claimNext(t, st, "w2"); ok {
			t.Fatal("another worker took a renewed lease")
		}
	})

	t.Run("release keeps the fence monotonic", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		for i := 1; i <= 3; i++ {
			addJob(t, st, "u1", "j")
			c := mustClaim(t, st, "w1")
			if c.Fence != int64(i) {
				t.Fatalf("claim %d has fence %d", i, c.Fence)
			}
			// a stale holder of the previous fence can never match the new one
			if i > 1 {
				if err := saveCheckpoint(st, "u1", c.Job.ID, int64(i-1)); !errors.Is(err, store.ErrFenceLost) {
					t.Errorf("old fence accepted: %v", err)
				}
			}
			release(t, st, c, store.JobSucceeded)
			clk.Advance(time.Second)
		}
	})

	t.Run("a stale worker cannot write after release", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		release(t, st, c, store.JobWaitingApproval)
		if err := saveCheckpoint(st, "u1", j.ID, c.Fence); err == nil {
			t.Fatal("SaveCheckpoint succeeded after release")
		}
		if got := getJob(t, st, "u1", j.ID); got.State != store.JobWaitingApproval {
			t.Errorf("state = %s", got.State)
		}
	})

	t.Run("release after cancel reports not applied and frees the lease", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j1 := addJob(t, st, "u1", "j1")
		clk.Advance(time.Second)
		j2 := addJob(t, st, "u1", "j2")
		c1 := mustClaim(t, st, "w1")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j1.ID) })

		if release(t, st, c1, store.JobSucceeded) {
			t.Error("release was applied to a cancelled job")
		}
		if got := getJob(t, st, "u1", j1.ID); got.State != store.JobCancelled {
			t.Errorf("state = %s, want cancelled", got.State)
		}
		if c2 := mustClaim(t, st, "w2"); c2.Job.ID != j2.ID {
			t.Errorf("claimed %s, want j2", c2.Job.ID)
		}
	})

	t.Run("terminal jobs reject writes and cancels", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		if err := saveCheckpoint(st, "u1", j.ID, c.Fence); !errors.Is(err, store.ErrConflict) {
			t.Errorf("SaveCheckpoint on a cancelled job: err = %v, want ErrConflict", err)
		}
		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) }); !errors.Is(err, store.ErrConflict) {
			t.Errorf("second CancelJob: err = %v, want ErrConflict", err)
		}
		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.CancelJob("u1", "nope") }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("CancelJob(unknown): err = %v, want ErrNotFound", err)
		}
	})

	t.Run("checkpoints persist and are validated", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")
		if err := st.InTx(bg(), func(tx store.Tx) error {
			return tx.SaveCheckpoint("u1", j.ID, json.RawMessage(`{"step":3}`), c.Fence)
		}); err != nil {
			t.Fatal(err)
		}
		if got := getJob(t, st, "u1", j.ID); string(got.Checkpoint) != `{"step":3}` {
			t.Errorf("checkpoint = %s", got.Checkpoint)
		}
		for _, bad := range []json.RawMessage{nil, json.RawMessage(`{nope`)} {
			err := st.InTx(bg(), func(tx store.Tx) error { return tx.SaveCheckpoint("u1", j.ID, bad, c.Fence) })
			if err == nil {
				t.Errorf("SaveCheckpoint(%q) succeeded", bad)
			}
		}
	})

	t.Run("steps get sequence numbers and need the lease", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "j")
		c := mustClaim(t, st, "w1")

		var s1, s2 store.JobStep
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			if s1, err = tx.AppendStep(store.JobStep{JobID: j.ID, Kind: "llm", Status: "ok", TokensIn: 10, TokensOut: 5, Summary: "plan"}, c.Fence); err != nil {
				return err
			}
			s2, err = tx.AppendStep(store.JobStep{JobID: j.ID, Kind: "tool", Tool: "files.list", CallID: "call-1", Status: "ok"}, c.Fence)
			return err
		})
		if s1.N != 1 || s2.N != 2 {
			t.Fatalf("step numbers = %d, %d", s1.N, s2.N)
		}
		var steps []store.JobStep
		_ = st.View(bg(), func(tx store.Tx) error {
			var err error
			steps, err = tx.ListSteps("u1", j.ID)
			return err
		})
		if len(steps) != 2 || steps[0].Kind != "llm" || steps[0].TokensIn != 10 || steps[1].Tool != "files.list" {
			t.Errorf("steps = %+v", steps)
		}
		_ = st.View(bg(), func(tx store.Tx) error {
			other, err := tx.ListSteps("u2", j.ID)
			if err != nil || len(other) != 0 {
				t.Errorf("another user sees steps: %+v, %v", other, err)
			}
			return nil
		})

		bad := []store.JobStep{{JobID: j.ID, Kind: "weird", Status: "ok"}, {JobID: j.ID, Kind: "tool"}}
		for _, s := range bad {
			if err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.AppendStep(s, c.Fence); return err }); err == nil {
				t.Errorf("AppendStep(%+v) succeeded", s)
			}
		}
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.AppendStep(store.JobStep{JobID: "nope", Kind: "tool", Status: "ok"}, c.Fence)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown job: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("reaper requeues with backoff, then fails poison jobs", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "poison")
		reap := func() store.ReapResult {
			var r store.ReapResult
			mustTx(t, st, func(tx store.Tx) error {
				var err error
				r, err = tx.ReapExpired(2, backoffMinutes)
				return err
			})
			return r
		}

		c1 := mustClaim(t, st, "w1")
		if r := reap(); r.Requeued != 0 || r.Failed != 0 {
			t.Fatalf("reaped a live lease: %+v", r)
		}
		clk.Advance(3 * time.Minute)
		if r := reap(); r.Requeued != 1 || r.Failed != 0 {
			t.Fatalf("first reap = %+v", r)
		}
		got := getJob(t, st, "u1", j.ID)
		if got.State != store.JobQueued || got.Attempt != 1 || !got.NotBefore.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("after first reap: %+v", got)
		}
		if err := saveCheckpoint(st, "u1", j.ID, c1.Fence); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("a reaped worker can still write: err = %v", err)
		}
		if _, ok := claimNext(t, st, "w2"); ok {
			t.Fatal("claimed during backoff")
		}
		clk.Advance(time.Minute)
		c2 := mustClaim(t, st, "w2")
		if c2.Job.Attempt != 2 || c2.Fence != 2 {
			t.Fatalf("second claim = %+v", c2)
		}
		clk.Advance(3 * time.Minute)
		if r := reap(); r.Requeued != 0 || r.Failed != 1 {
			t.Fatalf("second reap = %+v", r)
		}
		got = getJob(t, st, "u1", j.ID)
		if got.State != store.JobFailed || got.Error != "max attempts exceeded" {
			t.Fatalf("after second reap: %+v", got)
		}
		if _, ok := claimNext(t, st, "w3"); ok {
			t.Fatal("a failed job was claimed again")
		}
	})

	t.Run("reaper also catches a running job whose lease moved to another job", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j1 := addJob(t, st, "u1", "j1")
		mustClaim(t, st, "w1")
		clk.Advance(3 * time.Minute)
		clk.Advance(time.Second)
		j2 := addJob(t, st, "u1", "j2")
		c2 := mustClaim(t, st, "w2")
		if c2.Job.ID != j2.ID {
			t.Fatalf("claimed %s, want j2", c2.Job.ID)
		}
		var r store.ReapResult
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			r, err = tx.ReapExpired(3, backoffMinutes)
			return err
		})
		if r.Requeued != 1 || r.Failed != 0 {
			t.Fatalf("reap = %+v", r)
		}
		if got := getJob(t, st, "u1", j1.ID); got.State != store.JobQueued {
			t.Errorf("j1 state = %s, want queued", got.State)
		}
		if got := getJob(t, st, "u1", j2.ID); got.State != store.JobRunning {
			t.Errorf("j2 state = %s, want running", got.State)
		}
		if err := saveCheckpoint(st, "u1", j2.ID, c2.Fence); err != nil {
			t.Errorf("the live worker lost its lease to the reaper: %v", err)
		}
	})

	t.Run("concurrent claimers never share a user", func(t *testing.T) {
		st, _ := open(t, f)
		users := []string{"u1", "u2", "u3", "u4", "u5"}
		for _, u := range users {
			mustUser(t, st, u)
			for i := 0; i < 4; i++ {
				addJob(t, st, u, "job")
			}
		}
		var mu sync.Mutex
		var claims []store.Claim
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for {
					var c store.Claim
					var ok bool
					err := st.InTx(bg(), func(tx store.Tx) error {
						var err error
						c, ok, err = tx.ClaimNextJob("worker", leaseTTL)
						return err
					})
					if err != nil {
						t.Errorf("claim: %v", err)
						return
					}
					if !ok {
						return
					}
					mu.Lock()
					claims = append(claims, c)
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		if len(claims) != len(users) {
			t.Fatalf("got %d claims, want one per user (%d)", len(claims), len(users))
		}
		seenUser, seenJob := map[string]bool{}, map[string]bool{}
		for _, c := range claims {
			if seenUser[c.Job.UserID] || seenJob[c.Job.ID] {
				t.Fatalf("duplicate claim: %+v", c)
			}
			seenUser[c.Job.UserID], seenJob[c.Job.ID] = true, true
		}
	})
}
