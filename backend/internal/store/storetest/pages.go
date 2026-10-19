package storetest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Pages", runPages) }

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
