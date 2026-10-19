package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() {
	register("Pages", runPages)
	register("PageLimits", runPages2)
	register("ApprovalLookups", runApprovalLookups)
}

// runPages covers keyset pagination (Before) of ListJobs, ListJobSummaries
// and ListApprovals, and the job summaries' projection.
func runPages(t *testing.T, f Factory) {
	t.Run("jobs before a cursor, ties broken by id", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		var all []store.Job // oldest first
		for i := range 6 {
			all = append(all, addJob(t, st, "u1", fmt.Sprintf("j%d", i)))
			if i%2 == 1 { // pairs share a created_at
				clk.Advance(time.Second)
			}
		}
		addJob(t, st, "u2", "other")
		// the store's order among equal created_at is id DESC
		listed := jobIDs(listJobs(t, st, "u1", store.JobFilter{}))
		for i := range all {
			cur := all[i]
			before := &store.PageKey{At: cur.CreatedAt, ID: cur.ID}
			pos := indexOf(listed, cur.ID)
			if got := jobIDs(listJobs(t, st, "u1", store.JobFilter{Before: before})); !equal(got, listed[pos+1:]) {
				t.Errorf("before %d = %v, want %v", i, got, listed[pos+1:])
			}
			var sum []string
			for _, s := range listSummaries(t, st, "u1", store.JobFilter{Before: before}) {
				sum = append(sum, s.ID)
			}
			if !equal(sum, listed[pos+1:]) {
				t.Errorf("summaries before %d = %v, want %v", i, sum, listed[pos+1:])
			}
		}
		// the cursor row itself may change state: its key is stable
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", all[3].ID) })
		pos := indexOf(listed, all[3].ID)
		got := jobIDs(listJobs(t, st, "u1", store.JobFilter{States: []store.JobState{store.JobQueued},
			Before: &store.PageKey{At: all[3].CreatedAt, ID: all[3].ID}}))
		if !equal(got, listed[pos+1:]) {
			t.Errorf("after a state change = %v, want %v", got, listed[pos+1:])
		}
		if got := jobIDs(listJobs(t, st, "u1", store.JobFilter{Limit: 2, Before: &store.PageKey{At: all[5].CreatedAt, ID: all[5].ID}})); len(got) != 2 {
			t.Errorf("limit = %v", got)
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.ListJobs("u1", store.JobFilter{Before: &store.PageKey{At: all[0].CreatedAt}})
			if !errors.Is(err, store.ErrInvalid) {
				return fmt.Errorf("ListJobs empty cursor id: %v", err)
			}
			_, err = tx.ListJobSummaries("u1", store.JobFilter{Before: &store.PageKey{ID: all[0].ID}})
			if !errors.Is(err, store.ErrInvalid) {
				return fmt.Errorf("ListJobSummaries zero cursor time: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})

	t.Run("job summaries carry the step count and no checkpoint", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		addJob(t, st, "u2", "other")
		c, ok := claimNext(t, st, "w")
		if !ok {
			t.Fatal("nothing to claim")
		}
		_ = c // u2's job
		clk.Advance(time.Second)
		a := addJob(t, st, "u1", "a")
		ca := mustClaim(t, st, "w1")
		if ca.Job.ID != a.ID {
			t.Fatalf("claimed %s", ca.Job.ID)
		}
		mustTx(t, st, func(tx store.Tx) error {
			for range 3 {
				if _, err := tx.AppendStep(store.JobStep{JobID: a.ID, Kind: "llm", Status: "ok"}, ca.Fence); err != nil {
					return err
				}
			}
			if err := tx.SaveCheckpoint("u1", a.ID, []byte(`{"v":1,"answer":"x"}`), ca.Fence); err != nil {
				return err
			}
			_, err := tx.ReleaseJob(ca, store.JobFailed, nil, "boom")
			return err
		})
		clk.Advance(time.Second)
		b := addJob(t, st, "u1", "b")
		got := listSummaries(t, st, "u1", store.JobFilter{})
		if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
			t.Fatalf("summaries = %+v", got)
		}
		s := got[1]
		if s.StepCount != 3 || got[0].StepCount != 0 || s.State != store.JobFailed || s.Error != "boom" || s.Goal != "a" ||
			s.UserID != "u1" || s.TriggerKind != "chat" || s.Runs != 1 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.NotBefore.IsZero() {
			t.Errorf("summary = %+v", s)
		}
		full := getJob(t, st, "u1", a.ID)
		if s.ID != full.ID || s.Attempt != full.Attempt || !s.CreatedAt.Equal(full.CreatedAt) || !s.UpdatedAt.Equal(full.UpdatedAt) {
			t.Errorf("summary %+v differs from job %+v", s, full)
		}
		if f := listSummaries(t, st, "u1", store.JobFilter{States: []store.JobState{store.JobQueued}}); len(f) != 1 || f[0].ID != b.ID {
			t.Errorf("filtered = %+v", f)
		}
		if l := listSummaries(t, st, "u1", store.JobFilter{Limit: 1}); len(l) != 1 || l[0].ID != b.ID {
			t.Errorf("limit = %+v", l)
		}
		if n := listSummaries(t, st, "nobody", store.JobFilter{}); len(n) != 0 {
			t.Errorf("unknown user = %+v", n)
		}
	})

	t.Run("approvals before a cursor", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		var all []store.Approval
		for i := range 6 {
			all = append(all, putApproval(t, st, c, approvalFor(c, clk, fmt.Sprint(i))))
			if i%2 == 1 {
				clk.Advance(time.Second)
			}
		}
		c2 := runningFor(t, st, "u2")
		putApproval(t, st, c2, approvalFor(c2, clk, "other"))
		listed := approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{}))
		if len(listed) != 6 {
			t.Fatalf("listed = %v", listed)
		}
		for i := range all {
			pos := indexOf(listed, all[i].ID)
			got := approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{Before: &store.PageKey{At: all[i].RequestedAt, ID: all[i].ID}}))
			if !equal(got, listed[pos+1:]) {
				t.Errorf("before %d = %v, want %v", i, got, listed[pos+1:])
			}
		}
		// decide the cursor row: pending rows after it are still found
		cur := all[3]
		if _, err := decideApproval(st, "u1", cur.ID, store.ApprovalRejected, cur.ArgsHash); err != nil {
			t.Fatal(err)
		}
		pos := indexOf(listed, cur.ID)
		got := approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{States: []store.ApprovalState{store.ApprovalPending},
			JobID: c.Job.ID, Before: &store.PageKey{At: cur.RequestedAt, ID: cur.ID}}))
		if !equal(got, listed[pos+1:]) {
			t.Errorf("after deciding the cursor = %v, want %v", got, listed[pos+1:])
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.ListApprovals("u1", store.ApprovalFilter{Before: &store.PageKey{At: cur.RequestedAt}})
			if !errors.Is(err, store.ErrInvalid) {
				return fmt.Errorf("empty cursor id: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})
}

func runPages2(t *testing.T, f Factory) {
	t.Run("job status is a light lookup of state and update time", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j := addJob(t, st, "u1", "a")
		clk.Advance(time.Second)
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		full := getJob(t, st, "u1", j.ID)
		err := st.View(bg(), func(tx store.Tx) error {
			js, err := tx.GetJobStatus("u1", j.ID)
			if err != nil {
				return err
			}
			if js.State != store.JobCancelled || !js.UpdatedAt.Equal(full.UpdatedAt) || !js.CreatedAt.Equal(full.CreatedAt) ||
				js.CreatedAt.Equal(js.UpdatedAt) {
				return fmt.Errorf("status = %+v, job = %+v", js, full)
			}
			if _, err := tx.GetJobStatus("u2", j.ID); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("another user's job: %v", err)
			}
			if _, err := tx.GetJobStatus("u1", "nope"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown job: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})

	t.Run("approval lists stop at a byte budget of args and preview", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		var ids []string // newest first after the loop
		for i := range 5 {
			a := approvalFor(c, clk, fmt.Sprint(i))
			a.Args = json.RawMessage(`{"path":"/` + strings.Repeat("x", 1000) + `"}`) // ~1 KiB
			a.Preview = json.RawMessage(`{}`)
			ids = append([]string{putApproval(t, st, c, a).ID}, ids...)
			clk.Advance(time.Second)
		}
		size := len(`{"path":"/`+strings.Repeat("x", 1000)+`"}`) + len(`{}`)
		for _, tc := range []struct {
			budget int64
			want   int
		}{{0, 5}, {1, 1}, {int64(size), 1}, {int64(size) + 1, 2}, {int64(3 * size), 3}, {1 << 30, 5}} {
			got := approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{MaxBytes: tc.budget}))
			if !equal(got, ids[:tc.want]) {
				t.Errorf("budget %d = %d rows, want %d", tc.budget, len(got), tc.want)
			}
		}
		// the budget combines with the limit and the cursor
		got := approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{MaxBytes: int64(3 * size), Limit: 2}))
		if !equal(got, ids[:2]) {
			t.Errorf("limit 2 = %v", got)
		}
		first := getApproval(t, st, "u1", ids[0])
		got = approvalIDs(listApprovalsF(t, st, "u1", store.ApprovalFilter{MaxBytes: int64(2 * size),
			Before: &store.PageKey{At: first.RequestedAt, ID: first.ID}}))
		if !equal(got, ids[1:3]) {
			t.Errorf("after the cursor = %v, want %v", got, ids[1:3])
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.ListApprovals("u1", store.ApprovalFilter{MaxBytes: -1})
			if !errors.Is(err, store.ErrInvalid) {
				return fmt.Errorf("negative budget: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})

	t.Run("a multi-page walk sees every row once while rows change", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		// all rows on ONE created_at: the order is the id alone, so a cursor
		// that compared only the time (col < ?) would end the walk early
		var want []string // newest first
		for i := range 10 {
			want = append([]string{addJob(t, st, "u1", fmt.Sprint(i)).ID}, want...)
		}
		want = jobIDs(listJobs(t, st, "u1", store.JobFilter{})) // the store's order (id DESC)
		var seen []string
		var before *store.PageKey
		for page := 0; ; page++ {
			list := listSummaries(t, st, "u1", store.JobFilter{Limit: 3, Before: before})
			if len(list) == 0 {
				break
			}
			for _, j := range list {
				seen = append(seen, j.ID)
			}
			last := list[len(list)-1]
			before = &store.PageKey{At: last.CreatedAt, ID: last.ID}
			// between pages: a row already seen and a row not yet seen change
			// state, and a new (newer) job arrives
			cancel := func(id string) {
				mustTx(t, st, func(tx store.Tx) error {
					if err := tx.CancelJob("u1", id); !errors.Is(err, store.ErrConflict) {
						return err // nil, or a real failure; already cancelled is fine
					}
					return nil
				})
			}
			cancel(list[0].ID)
			if idx := indexOf(want, last.ID); idx+1 < len(want) {
				cancel(want[idx+1])
			}
			clk.Advance(time.Second)
			addJob(t, st, "u1", fmt.Sprintf("new-%d", page))
			if page > 10 {
				t.Fatal("the walk does not end")
			}
		}
		if !equal(seen, want) {
			t.Errorf("walk = %v, want %v", seen, want)
		}

		// a state filter whose cursor row leaves the filter between pages:
		// queued jobs, the last of every page is cancelled before the next
		st, clk = open(t, f)
		mustUser(t, st, "u1")
		for i := range 9 {
			addJob(t, st, "u1", fmt.Sprint(i))
		}
		queued := []store.JobState{store.JobQueued}
		wantQ := jobIDs(listJobs(t, st, "u1", store.JobFilter{States: queued}))
		var seenQ []string
		var beforeQ *store.PageKey
		for page := 0; ; page++ {
			list := listSummaries(t, st, "u1", store.JobFilter{States: queued, Limit: 2, Before: beforeQ})
			if len(list) == 0 {
				break
			}
			for _, j := range list {
				seenQ = append(seenQ, j.ID)
			}
			last := list[len(list)-1]
			beforeQ = &store.PageKey{At: last.CreatedAt, ID: last.ID}
			mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", last.ID) })
			if page > 10 {
				t.Fatal("the filtered walk does not end")
			}
		}
		if !equal(seenQ, wantQ) {
			t.Errorf("filtered walk = %v, want %v", seenQ, wantQ)
		}

		st, clk = open(t, f) // a fresh store: claiming must find u2's job
		c := runningFor(t, st, "u2")
		for i := range 7 { // one requested_at for all
			putApproval(t, st, c, approvalFor(c, clk, fmt.Sprint(i)))
		}
		pending := []store.ApprovalState{store.ApprovalPending}
		wantA := approvalIDs(listApprovalsF(t, st, "u2", store.ApprovalFilter{States: pending}))
		var seenA []string
		var beforeA *store.PageKey
		for page := 0; ; page++ {
			// pending only: the cursor row is decided (leaves the filter) before the next page
			list := listApprovalsF(t, st, "u2", store.ApprovalFilter{States: pending, Limit: 2, Before: beforeA})
			if len(list) == 0 {
				break
			}
			for _, a := range list {
				seenA = append(seenA, a.ID)
			}
			last := list[len(list)-1]
			beforeA = &store.PageKey{At: last.RequestedAt, ID: last.ID}
			if _, err := decideApproval(st, "u2", last.ID, store.ApprovalRejected, last.ArgsHash); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Second)
			putApproval(t, st, c, approvalFor(c, clk, fmt.Sprintf("new-%d", page)))
			if page > 10 {
				t.Fatal("the walk does not end")
			}
		}
		if !equal(seenA, wantA) {
			t.Errorf("approval walk = %v, want %v", seenA, wantA)
		}
	})
}

func runApprovalLookups(t *testing.T, f Factory) {
	t.Run("pending approvals of live jobs are counted without loading them", func(t *testing.T) {
		st, clk := open(t, f)
		c1 := runningFor(t, st, "u1")
		putApproval(t, st, c1, approvalFor(c1, clk, "ended"))
		mustTx(t, st, func(tx store.Tx) error {
			if _, err := tx.ReleaseJob(c1, store.JobWaitingApproval, nil, ""); err != nil {
				return err
			}
			return tx.CancelJob("u1", c1.Job.ID) // its approval stays pending
		})
		addJob(t, st, "u1", "live")
		c2 := mustClaim(t, st, "w2")
		putApproval(t, st, c2, approvalFor(c2, clk, "a"))
		putApproval(t, st, c2, approvalFor(c2, clk, "b"))
		rej := putApproval(t, st, c2, approvalFor(c2, clk, "c"))
		if _, err := decideApproval(st, "u1", rej.ID, store.ApprovalRejected, rej.ArgsHash); err != nil {
			t.Fatal(err)
		}
		c3 := runningFor(t, st, "u2")
		putApproval(t, st, c3, approvalFor(c3, clk, "other"))
		err := st.View(bg(), func(tx store.Tx) error {
			for user, want := range map[string]int{"u1": 2, "u2": 1, "nobody": 0} {
				n, err := tx.CountPendingOfLiveJobs(user)
				if err != nil {
					return err
				}
				if n != want {
					return fmt.Errorf("%s: %d, want %d", user, n, want)
				}
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		// once the live job ends too, nothing is decidable
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", c2.Job.ID) })
		_ = st.View(bg(), func(tx store.Tx) error {
			if n, err := tx.CountPendingOfLiveJobs("u1"); err != nil || n != 0 {
				t.Errorf("after the job ended: %d %v", n, err)
			}
			return nil
		})
	})

	t.Run("an approval's page key is a light lookup", func(t *testing.T) {
		st, clk := open(t, f)
		c := runningFor(t, st, "u1")
		a := putApproval(t, st, c, approvalFor(c, clk, "k"))
		runningFor(t, st, "u2")
		err := st.View(bg(), func(tx store.Tx) error {
			k, err := tx.GetApprovalKey("u1", a.ID)
			if err != nil {
				return err
			}
			if k.ID != a.ID || !k.At.Equal(a.RequestedAt) {
				return fmt.Errorf("key = %+v, approval at %v", k, a.RequestedAt)
			}
			if _, err := tx.GetApprovalKey("u2", a.ID); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("another user's approval: %v", err)
			}
			if _, err := tx.GetApprovalKey("u1", "nope"); !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown approval: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})
}

func listJobs(t *testing.T, st store.Store, user string, f store.JobFilter) []store.Job {
	t.Helper()
	var out []store.Job
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListJobs(user, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func listSummaries(t *testing.T, st store.Store, user string, f store.JobFilter) []store.JobSummary {
	t.Helper()
	var out []store.JobSummary
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListJobSummaries(user, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func listApprovalsF(t *testing.T, st store.Store, user string, f store.ApprovalFilter) []store.Approval {
	t.Helper()
	var out []store.Approval
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListApprovals(user, f)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func jobIDs(js []store.Job) []string {
	var out []string
	for _, j := range js {
		out = append(out, j.ID)
	}
	return out
}

func approvalIDs(as []store.Approval) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func indexOf(list []string, id string) int {
	for i, v := range list {
		if v == id {
			return i
		}
	}
	return -1
}

func equal(a, b []string) bool {
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
