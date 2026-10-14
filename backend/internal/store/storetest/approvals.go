package storetest

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Approvals", runApprovals) }

const approvalTTL = 7 * 24 * time.Hour

// runningFor enrols the user, queues a job and claims it.
func runningFor(t *testing.T, st store.Store, user string) store.Claim {
	t.Helper()
	mustUser(t, st, user)
	addJob(t, st, user, "job for "+user)
	return mustClaim(t, st, "w-"+user)
}

func approvalFor(c store.Claim, clk *Clock, key string) store.Approval {
	return store.Approval{
		UserID:     c.Job.UserID,
		JobID:      c.Job.ID,
		CallID:     "call-" + key,
		Tool:       "files.delete",
		Risk:       store.RiskDestructive,
		Args:       json.RawMessage(`{"path":"/old.txt"}`),
		ArgsHash:   "sha256:abc",
		TargetETag: `"etag-1"`,
		Rationale:  "tidy up",
		Preview:    json.RawMessage(`{"paths":["/old.txt"]}`),
		IdemKey:    "idem-" + key,
		ExpiresAt:  clk.Now().Add(approvalTTL),
	}
}

func putApproval(t *testing.T, st store.Store, c store.Claim, a store.Approval) store.Approval {
	t.Helper()
	var out store.Approval
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		out, err = tx.InsertApproval(a, c.Fence)
		return err
	})
	return out
}

func getApproval(t *testing.T, st store.Store, user, id string) store.Approval {
	t.Helper()
	var a store.Approval
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(user, id)
		return err
	})
	if err != nil {
		t.Fatalf("GetApproval(%s): %v", id, err)
	}
	return a
}

func decideApproval(st store.Store, user, id string, d store.ApprovalState, hash string) (store.Approval, error) {
	var a store.Approval
	err := st.InTx(bg(), func(tx store.Tx) error {
		var err error
		a, err = tx.DecideApproval(user, id, d, "alice", hash)
		return err
	})
	return a, err
}

func transition(st store.Store, user, id string, from, to store.ApprovalState, result string) (store.Approval, error) {
	var a store.Approval
	err := st.InTx(bg(), func(tx store.Tx) error {
		var err error
		a, err = tx.TransitionApproval(user, id, from, to, store.ApprovalPatch{Result: json.RawMessage(result)})
		return err
	})
	return a, err
}

func resume(t *testing.T, st store.Store, user, job string) bool {
	t.Helper()
	var ok bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		ok, err = tx.ResumeJobIfSettled(user, job)
		return err
	})
	return ok
}

func expireApprovals(t *testing.T, st store.Store) []store.ApprovalRef {
	t.Helper()
	var refs []store.ApprovalRef
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		refs, err = tx.ExpireApprovals()
		return err
	})
	return refs
}

// waitFor inserts n pending approvals for the claimed job and releases the
// job into waiting_approval in one transaction, as the gate does.
func waitFor(t *testing.T, st store.Store, clk *Clock, c store.Claim, n int) []store.Approval {
	t.Helper()
	out := make([]store.Approval, 0, n)
	mustTx(t, st, func(tx store.Tx) error {
		for i := 0; i < n; i++ {
			a, err := tx.InsertApproval(approvalFor(c, clk, string(rune('a'+i))), c.Fence)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		_, err := tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		return err
	})
	return out
}

func runApprovals(t *testing.T, f Factory) {
	t.Run("insert applies defaults and round trips", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		if a.ID == "" || a.State != store.ApprovalPending || !a.RequestedAt.Equal(clk.Now()) ||
			a.DecidedAt != nil || a.ExecutedAt != nil || a.DecidedBy != "" || len(a.Result) != 0 {
			t.Fatalf("inserted = %+v", a)
		}
		got := getApproval(t, st, "u1", a.ID)
		if got.Tool != "files.delete" || got.Risk != store.RiskDestructive || string(got.Args) != `{"path":"/old.txt"}` ||
			got.ArgsHash != "sha256:abc" || got.TargetETag != `"etag-1"` || got.Rationale != "tidy up" ||
			string(got.Preview) != `{"paths":["/old.txt"]}` || got.IdemKey != "idem-1" || got.CallID != "call-1" ||
			!got.ExpiresAt.Equal(clk.Now().Add(approvalTTL)) {
			t.Errorf("round trip = %+v", got)
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetApproval("u2", a.ID)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's GetApproval: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("insert defaults an empty preview to an empty object", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		in := approvalFor(c, clk, "1")
		in.Preview = nil
		a := putApproval(t, st, c, in)
		if string(getApproval(t, st, "u1", a.ID).Preview) != "{}" {
			t.Errorf("preview = %s", getApproval(t, st, "u1", a.ID).Preview)
		}
	})

	t.Run("insert needs the lease", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		insert := func(fence int64, mod func(*store.Approval)) error {
			a := approvalFor(c, clk, "x")
			if mod != nil {
				mod(&a)
			}
			return st.InTx(bg(), func(tx store.Tx) error { _, err := tx.InsertApproval(a, fence); return err })
		}
		if err := insert(c.Fence+1, nil); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("wrong fence: err = %v, want ErrFenceLost", err)
		}
		if err := insert(c.Fence, func(a *store.Approval) { a.JobID = "nope" }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown job: err = %v, want ErrNotFound", err)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", c.Job.ID) })
		if err := insert(c.Fence, nil); !errors.Is(err, store.ErrConflict) {
			t.Errorf("cancelled job: err = %v, want ErrConflict", err)
		}
	})

	t.Run("insert after the lease was released fails", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		release(t, st, c, store.JobWaitingApproval)
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.InsertApproval(approvalFor(c, clk, "x"), c.Fence)
			return err
		})
		if !errors.Is(err, store.ErrFenceLost) {
			t.Fatalf("err = %v, want ErrFenceLost", err)
		}
	})

	t.Run("insert validation", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		bad := map[string]func(*store.Approval){
			"no user":         func(a *store.Approval) { a.UserID = "" },
			"no job":          func(a *store.Approval) { a.JobID = "" },
			"no call":         func(a *store.Approval) { a.CallID = "" },
			"no tool":         func(a *store.Approval) { a.Tool = "" },
			"read risk":       func(a *store.Approval) { a.Risk = "read" },
			"no risk":         func(a *store.Approval) { a.Risk = "" },
			"no args":         func(a *store.Approval) { a.Args = nil },
			"broken args":     func(a *store.Approval) { a.Args = json.RawMessage(`{nope`) },
			"no args hash":    func(a *store.Approval) { a.ArgsHash = "" },
			"no idem key":     func(a *store.Approval) { a.IdemKey = "" },
			"broken preview":  func(a *store.Approval) { a.Preview = json.RawMessage(`{nope`) },
			"already expired": func(a *store.Approval) { a.ExpiresAt = clk.Now() },
		}
		for name, mod := range bad {
			a := approvalFor(c, clk, "x")
			mod(&a)
			err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.InsertApproval(a, c.Fence); return err })
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
		}
	})

	t.Run("the idempotency key is unique", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		putApproval(t, st, c, approvalFor(c, clk, "1"))
		dup := approvalFor(c, clk, "2")
		dup.IdemKey = "idem-1"
		err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.InsertApproval(dup, c.Fence); return err })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("list filters and orders newest first", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a1 := putApproval(t, st, c, approvalFor(c, clk, "1"))
		clk.Advance(time.Second)
		a2 := putApproval(t, st, c, approvalFor(c, clk, "2"))
		clk.Advance(time.Second)
		a3 := putApproval(t, st, c, approvalFor(c, clk, "3"))
		if _, err := decideApproval(st, "u1", a2.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}

		list := func(f store.ApprovalFilter) []store.Approval {
			var out []store.Approval
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				out, err = tx.ListApprovals("u1", f)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if all := list(store.ApprovalFilter{}); len(all) != 3 || all[0].ID != a3.ID || all[1].ID != a2.ID || all[2].ID != a1.ID {
			t.Errorf("all = %+v", all)
		}
		if p := list(store.ApprovalFilter{States: []store.ApprovalState{store.ApprovalPending}}); len(p) != 2 || p[0].ID != a3.ID || p[1].ID != a1.ID {
			t.Errorf("pending = %+v", p)
		}
		if one := list(store.ApprovalFilter{Limit: 1}); len(one) != 1 || one[0].ID != a3.ID {
			t.Errorf("limit 1 = %+v", one)
		}
		if other := list(store.ApprovalFilter{JobID: "other-job"}); len(other) != 0 {
			t.Errorf("another job = %+v", other)
		}
		if byJob := list(store.ApprovalFilter{JobID: c.Job.ID}); len(byJob) != 3 {
			t.Errorf("by job = %+v", byJob)
		}
	})

	t.Run("decide approves or rejects a pending approval", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		b := putApproval(t, st, c, approvalFor(c, clk, "2"))
		clk.Advance(time.Minute)

		got, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc")
		if err != nil || got.State != store.ApprovalApproved || got.DecidedBy != "alice" || got.DecidedAt == nil || !got.DecidedAt.Equal(clk.Now()) {
			t.Fatalf("approve = %+v, %v", got, err)
		}
		if stored := getApproval(t, st, "u1", a.ID); stored.State != store.ApprovalApproved || stored.DecidedBy != "alice" {
			t.Errorf("stored = %+v", stored)
		}
		got, err = decideApproval(st, "u1", b.ID, store.ApprovalRejected, "sha256:abc")
		if err != nil || got.State != store.ApprovalRejected {
			t.Fatalf("reject = %+v, %v", got, err)
		}
	})

	t.Run("decide refuses what it must refuse", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		mustUser(t, st, "u2")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))

		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:other"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("wrong args hash: err = %v, want ErrInvalid", err)
		}
		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalExecuting, "sha256:abc"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("bad decision: err = %v, want ErrInvalid", err)
		}
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.DecideApproval("u1", a.ID, store.ApprovalApproved, "", "sha256:abc")
			return err
		})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("empty decidedBy: err = %v, want ErrInvalid", err)
		}
		if _, err := decideApproval(st, "u1", "nope", store.ApprovalApproved, "sha256:abc"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown id: err = %v, want ErrNotFound", err)
		}
		if _, err := decideApproval(st, "u2", a.ID, store.ApprovalApproved, "sha256:abc"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user: err = %v, want ErrNotFound", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalPending {
			t.Fatalf("a refused decision changed the state to %s", got.State)
		}

		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}
		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalRejected, "sha256:abc"); !errors.Is(err, store.ErrConflict) {
			t.Errorf("deciding twice: err = %v, want ErrConflict", err)
		}
	})

	t.Run("deciding after expiry reports expired and changes nothing", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		clk.Advance(approvalTTL)
		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); !errors.Is(err, store.ErrExpired) {
			t.Fatalf("err = %v, want ErrExpired", err)
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalPending {
			t.Fatalf("state = %s, want pending", got.State)
		}
	})

	// The store has a single writer, so the two transactions are serialised and
	// the loser sees the winner's committed state; this proves the
	// compare-and-swap outcome, not a race inside one transaction.
	t.Run("two concurrent decisions have exactly one winner", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		decisions := []store.ApprovalState{store.ApprovalApproved, store.ApprovalRejected}
		errs := make([]error, len(decisions))
		var wg sync.WaitGroup
		for i, d := range decisions {
			wg.Add(1)
			go func(i int, d store.ApprovalState) {
				defer wg.Done()
				_, errs[i] = decideApproval(st, "u1", a.ID, d, "sha256:abc")
			}(i, d)
		}
		wg.Wait()
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil && winner == -1:
				winner = i
			case err == nil:
				t.Fatal("both decisions succeeded")
			case !errors.Is(err, store.ErrConflict):
				t.Fatalf("loser err = %v, want ErrConflict", err)
			}
		}
		if winner == -1 {
			t.Fatal("no decision succeeded")
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != decisions[winner] {
			t.Fatalf("state = %s, want %s", got.State, decisions[winner])
		}
	})

	t.Run("worker transitions are compare-and-swap on legal edges", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}

		running, err := transition(st, "u1", a.ID, store.ApprovalApproved, store.ApprovalExecuting, "")
		if err != nil || running.State != store.ApprovalExecuting || running.ExecutedAt != nil {
			t.Fatalf("approved -> executing = %+v, %v", running, err)
		}
		if _, err := transition(st, "u1", a.ID, store.ApprovalApproved, store.ApprovalExecuting, ""); !errors.Is(err, store.ErrConflict) {
			t.Errorf("stale from-state: err = %v, want ErrConflict", err)
		}
		clk.Advance(time.Minute)
		done, err := transition(st, "u1", a.ID, store.ApprovalExecuting, store.ApprovalExecuted, `{"ok":true}`)
		if err != nil || done.State != store.ApprovalExecuted || string(done.Result) != `{"ok":true}` ||
			done.ExecutedAt == nil || !done.ExecutedAt.Equal(clk.Now()) {
			t.Fatalf("executing -> executed = %+v, %v", done, err)
		}
		if _, err := transition(st, "u1", a.ID, store.ApprovalExecuted, store.ApprovalExecuting, ""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("leaving a terminal state: err = %v, want ErrInvalid", err)
		}
	})

	t.Run("only the worker edges are allowed", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		illegal := []struct{ from, to store.ApprovalState }{
			{store.ApprovalPending, store.ApprovalApproved},
			{store.ApprovalPending, store.ApprovalRejected},
			{store.ApprovalPending, store.ApprovalExecuting},
			{store.ApprovalPending, store.ApprovalExpired},
			{store.ApprovalApproved, store.ApprovalApproved},
			{store.ApprovalApproved, store.ApprovalExecuted},
			{store.ApprovalExecuting, store.ApprovalApproved},
			{store.ApprovalRejected, store.ApprovalExecuting},
			{"weird", store.ApprovalExecuting},
		}
		for _, e := range illegal {
			if _, err := transition(st, "u1", a.ID, e.from, e.to, ""); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s -> %s: err = %v, want ErrInvalid", e.from, e.to, err)
			}
		}
		if got := getApproval(t, st, "u1", a.ID); got.State != store.ApprovalPending {
			t.Fatalf("an illegal transition changed the state to %s", got.State)
		}
		if _, err := transition(st, "u1", a.ID, store.ApprovalApproved, store.ApprovalExecuting, `{broken`); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("broken result JSON: err = %v, want ErrInvalid", err)
		}
		if _, err := transition(st, "u1", "nope", store.ApprovalApproved, store.ApprovalExecuting, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown id: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("the other legal worker edges work", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		approve := func(key string) store.Approval {
			a := putApproval(t, st, c, approvalFor(c, clk, key))
			if _, err := decideApproval(st, "u1", a.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
				t.Fatal(err)
			}
			return a
		}
		a, b, d := approve("1"), approve("2"), approve("3")
		if _, err := transition(st, "u1", a.ID, store.ApprovalApproved, store.ApprovalExpired, ""); err != nil {
			t.Errorf("approved -> expired: %v", err)
		}
		if _, err := transition(st, "u1", b.ID, store.ApprovalApproved, store.ApprovalFailed, `{"error":"etag changed"}`); err != nil {
			t.Errorf("approved -> failed: %v", err)
		}
		if _, err := transition(st, "u1", d.ID, store.ApprovalApproved, store.ApprovalExecuting, ""); err != nil {
			t.Fatal(err)
		}
		if got, err := transition(st, "u1", d.ID, store.ApprovalExecuting, store.ApprovalUnknown, ""); err != nil || got.ExecutedAt != nil {
			t.Errorf("executing -> unknown = %+v, %v (executed_at must stay unset)", got, err)
		}
	})

	t.Run("expiry sweeps only pending approvals past their deadline", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		early := approvalFor(c, clk, "early")
		early.ExpiresAt = clk.Now().Add(time.Hour)
		late := approvalFor(c, clk, "late")
		late.ExpiresAt = clk.Now().Add(10 * time.Hour)
		decided := approvalFor(c, clk, "decided")
		decided.ExpiresAt = clk.Now().Add(time.Hour)
		a := putApproval(t, st, c, early)
		b := putApproval(t, st, c, late)
		d := putApproval(t, st, c, decided)
		if _, err := decideApproval(st, "u1", d.ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}

		if refs := expireApprovals(t, st); len(refs) != 0 {
			t.Fatalf("nothing is due yet, got %+v", refs)
		}
		clk.Advance(2 * time.Hour)
		refs := expireApprovals(t, st)
		if len(refs) != 1 || refs[0].ID != a.ID || refs[0].UserID != "u1" || refs[0].JobID != c.Job.ID {
			t.Fatalf("refs = %+v", refs)
		}
		if getApproval(t, st, "u1", a.ID).State != store.ApprovalExpired ||
			getApproval(t, st, "u1", b.ID).State != store.ApprovalPending ||
			getApproval(t, st, "u1", d.ID).State != store.ApprovalApproved {
			t.Fatal("wrong states after the first sweep")
		}
		if again := expireApprovals(t, st); len(again) != 0 {
			t.Fatalf("a second sweep changed %+v", again)
		}
		clk.Advance(10 * time.Hour)
		if refs := expireApprovals(t, st); len(refs) != 1 || refs[0].ID != b.ID {
			t.Fatalf("second deadline: refs = %+v", refs)
		}
	})

	t.Run("a waiting job resumes once no approval is pending", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		as := waitFor(t, st, clk, c, 2)
		if got := getJob(t, st, "u1", c.Job.ID); got.State != store.JobWaitingApproval {
			t.Fatalf("job state = %s", got.State)
		}

		if resume(t, st, "u1", c.Job.ID) {
			t.Fatal("resumed with two approvals pending")
		}
		if _, err := decideApproval(st, "u1", as[0].ID, store.ApprovalApproved, "sha256:abc"); err != nil {
			t.Fatal(err)
		}
		if resume(t, st, "u1", c.Job.ID) {
			t.Fatal("resumed with one approval still pending")
		}
		if _, err := decideApproval(st, "u1", as[1].ID, store.ApprovalRejected, "sha256:abc"); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Second)
		if !resume(t, st, "u1", c.Job.ID) {
			t.Fatal("did not resume with nothing pending")
		}
		got := getJob(t, st, "u1", c.Job.ID)
		if got.State != store.JobQueued || !got.NotBefore.Equal(clk.Now()) {
			t.Fatalf("after resume: %+v", got)
		}
		if resume(t, st, "u1", c.Job.ID) {
			t.Fatal("resumed a job that is not waiting any more")
		}
		if again := mustClaim(t, st, "w2"); again.Job.ID != c.Job.ID {
			t.Fatalf("claimed %s", again.Job.ID)
		}
	})

	t.Run("expired approvals also settle a waiting job", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		waitFor(t, st, clk, c, 2)
		clk.Advance(approvalTTL)
		if refs := expireApprovals(t, st); len(refs) != 2 {
			t.Fatalf("refs = %+v", refs)
		}
		if !resume(t, st, "u1", c.Job.ID) {
			t.Fatal("did not resume after every approval expired")
		}
	})

	t.Run("resume leaves other jobs alone", func(t *testing.T) {
		st, _ := open(t, f)
		c := runningFor(t, st, "u1")
		if resume(t, st, "u1", c.Job.ID) {
			t.Fatal("resumed a running job")
		}
		mustUser(t, st, "u2")
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.ResumeJobIfSettled("u2", c.Job.ID)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's job: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("deleting the user removes the approvals", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "1"))
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("u1") })
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetApproval("u1", a.ID)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}
