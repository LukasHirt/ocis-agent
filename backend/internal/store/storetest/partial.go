package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("JobPartial", runJobPartial) }

func setPartial(st store.Store, c store.Claim, state store.PartialState, text string) error {
	return st.InTx(bg(), func(tx store.Tx) error {
		return tx.SetJobPartial(c.Job.UserID, c.Job.ID, c.Fence, state, text)
	})
}

func mustPartial(t *testing.T, st store.Store, c store.Claim, state store.PartialState, text string) {
	t.Helper()
	if err := setPartial(st, c, state, text); err != nil {
		t.Fatalf("SetJobPartial: %v", err)
	}
}

func wantNoPartial(t *testing.T, st store.Store, user, id, when string) {
	t.Helper()
	j := getJob(t, st, user, id)
	if j.JobPartial != (store.JobPartial{}) {
		t.Errorf("%s: partial = %+v, want it cleared", when, j.JobPartial)
	}
}

func changeMarks(t *testing.T, st store.Store, user string) store.ChangeMarks {
	t.Helper()
	var m store.ChangeMarks
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		m, err = tx.ChangeMarks(user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

func runJobPartial(t *testing.T, f Factory) {
	t.Run("a running job's partial is stored, read back and moves its updated_at", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "g")
		c := mustClaim(t, st, "w1")
		if got := getJob(t, st, "u1", j.ID); got.JobPartial != (store.JobPartial{}) {
			t.Fatalf("fresh claim: %+v", got.JobPartial)
		}
		clk.Advance(time.Second)
		mustPartial(t, st, c, store.PartialThinking, "")
		got := getJob(t, st, "u1", j.ID)
		if got.PartialState != store.PartialThinking || got.Partial != "" || !got.PartialAt.Equal(clk.Now()) || !got.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("thinking: %+v, updated %v", got.JobPartial, got.UpdatedAt)
		}
		clk.Advance(time.Second)
		text := "Hello ‮ world\x00 <b>"
		mustPartial(t, st, c, store.PartialWriting, text)
		got = getJob(t, st, "u1", j.ID)
		if got.PartialState != store.PartialWriting || got.Partial != text || !got.PartialAt.Equal(clk.Now()) || !got.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("writing: %+v", got.JobPartial)
		}
		if got.State != store.JobRunning || string(got.Checkpoint) != "{}" {
			t.Errorf("the job changed: %s %s", got.State, got.Checkpoint)
		}
		// the light lookups carry it too
		_ = viewErr(st, func(tx store.Tx) error {
			js, err := tx.GetJobStatus("u1", j.ID)
			if err != nil || js.JobPartial != got.JobPartial {
				t.Errorf("GetJobStatus = %+v, %v", js.JobPartial, err)
			}
			m, err := tx.JobStatuses("u1", []string{j.ID})
			if err != nil || m[j.ID].JobPartial != got.JobPartial {
				t.Errorf("JobStatuses = %+v, %v", m[j.ID].JobPartial, err)
			}
			sums, err := tx.ListJobSummaries("u1", store.JobFilter{})
			if err != nil || len(sums) != 1 || sums[0].JobPartial != got.JobPartial {
				t.Errorf("ListJobSummaries = %+v, %v", sums, err)
			}
			return nil
		})
		clk.Advance(time.Second)
		mustPartial(t, st, c, store.PartialNone, "")
		got = getJob(t, st, "u1", j.ID)
		if got.Partial != "" || got.PartialState != store.PartialNone || !got.PartialAt.Equal(clk.Now()) {
			t.Errorf("cleared: %+v", got.JobPartial)
		}
	})

	t.Run("SetJobPartial is fenced", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		j := addJob(t, st, "u1", "g")
		c := mustClaim(t, st, "w1")
		stale := c
		stale.Fence++
		if err := setPartial(st, stale, store.PartialWriting, "x"); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("wrong fence: %v, want ErrFenceLost", err)
		}
		other := c
		other.Job.UserID = "u2"
		if err := setPartial(st, other, store.PartialWriting, "x"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's job: %v, want ErrNotFound", err)
		}
		release(t, st, c, store.JobQueued)
		if err := setPartial(st, c, store.PartialWriting, "x"); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("after the release: %v, want ErrFenceLost", err)
		}
		// a takeover: the first worker's fence is stale
		c1 := mustClaim(t, st, "w1")
		clk.Advance(3 * time.Minute)
		mustTx(t, st, func(tx store.Tx) error { _, err := tx.ReapExpired(5, backoffMinutes); return err })
		clk.Advance(time.Hour)
		c2 := mustClaim(t, st, "w2")
		if err := setPartial(st, c1, store.PartialWriting, "x"); !errors.Is(err, store.ErrFenceLost) {
			t.Errorf("stale worker: %v, want ErrFenceLost", err)
		}
		mustPartial(t, st, c2, store.PartialWriting, "y")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		if err := setPartial(st, c2, store.PartialWriting, "z"); !errors.Is(err, store.ErrConflict) {
			t.Errorf("cancelled job: %v, want ErrConflict", err)
		}
		wantNoPartial(t, st, "u1", j.ID, "after the cancel")
	})

	t.Run("SetJobPartial validates its input", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		j := addJob(t, st, "u1", "g")
		c := mustClaim(t, st, "w1")
		for name, in := range map[string]struct {
			state store.PartialState
			text  string
		}{
			"unknown state":       {"dreaming", ""},
			"text without state":  {store.PartialNone, "x"},
			"too long":            {store.PartialWriting, strings.Repeat("a", store.MaxPartialBytes+1)},
			"invalid UTF-8":       {store.PartialWriting, "a\xffb"},
			"case variant state":  {"Writing", "x"},
			"thinking, too long":  {store.PartialThinking, strings.Repeat("a", store.MaxPartialBytes+1)},
			"surrogate half rune": {store.PartialWriting, "\xed\xa0\x80"},
		} {
			if err := setPartial(st, c, in.state, in.text); !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", name, err)
			}
		}
		mustPartial(t, st, c, store.PartialWriting, strings.Repeat("ä", store.MaxPartialBytes/2))
		if got := getJob(t, st, "u1", j.ID); len(got.Partial) != store.MaxPartialBytes {
			t.Errorf("at the cap: %d bytes", len(got.Partial))
		}
	})

	t.Run("every transition clears the partial", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		claimWith := func() store.Claim {
			t.Helper()
			c := mustClaim(t, st, "w1")
			mustPartial(t, st, c, store.PartialWriting, "half an answer")
			return c
		}
		for _, next := range []store.JobState{store.JobQueued, store.JobWaitingApproval, store.JobSucceeded, store.JobFailed, store.JobCancelled} {
			j := addJob(t, st, "u1", string(next))
			c := claimWith()
			mustTx(t, st, func(tx store.Tx) error { _, err := tx.ReleaseJob(c, next, nil, ""); return err })
			wantNoPartial(t, st, "u1", j.ID, "release to "+string(next))
			if next == store.JobQueued {
				mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
			}
			if next == store.JobWaitingApproval {
				mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
			}
		}

		j := addJob(t, st, "u1", "requeue")
		c := claimWith()
		if !requeue(t, st, c, clk.Now().Add(time.Minute)) {
			t.Fatal("requeue not applied")
		}
		wantNoPartial(t, st, "u1", j.ID, "requeue")
		clk.Advance(time.Minute)

		// the job is claimed again and starts without a partial
		c = mustClaim(t, st, "w1")
		wantNoPartial(t, st, "u1", j.ID, "a new claim")
		mustPartial(t, st, c, store.PartialThinking, "")
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", j.ID) })
		wantNoPartial(t, st, "u1", j.ID, "cancel of a running job")
		clk.Advance(3 * time.Minute) // the cancelled job's lease runs out

		// the reaper: requeue, then fail at the attempts cap
		j = addJob(t, st, "u1", "reaped")
		c = claimWith()
		clk.Advance(3 * time.Minute)
		var res store.ReapResult
		mustTx(t, st, func(tx store.Tx) error { var err error; res, err = tx.ReapExpired(2, backoffMinutes); return err })
		if res.Requeued != 1 {
			t.Fatalf("reap = %+v", res)
		}
		wantNoPartial(t, st, "u1", j.ID, "reaper requeue")
		clk.Advance(time.Hour)
		c = claimWith()
		if c.Job.ID != j.ID || c.Job.JobPartial != (store.JobPartial{}) {
			t.Fatalf("claimed %s with %+v", c.Job.ID, c.Job.JobPartial)
		}
		clk.Advance(3 * time.Minute)
		mustTx(t, st, func(tx store.Tx) error { var err error; res, err = tx.ReapExpired(2, backoffMinutes); return err })
		if res.Failed != 1 {
			t.Fatalf("reap = %+v", res)
		}
		wantNoPartial(t, st, "u1", j.ID, "reaper fail")
	})

	t.Run("a running chat job's partial moves the jobs and messages marks", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		addJob(t, st, "u1", "chat")
		c := mustClaim(t, st, "w1")
		base := changeMarks(t, st, "u1")
		other := changeMarks(t, st, "u2")
		clk.Advance(time.Second)
		mustPartial(t, st, c, store.PartialWriting, "a")
		m1 := changeMarks(t, st, "u1")
		if m1.JobsAt != clk.Now().UnixMilli() || m1.MessagesAt != clk.Now().UnixMilli() || m1.AuditSeq != base.AuditSeq || m1.PolicyAt != base.PolicyAt {
			t.Errorf("after a partial = %+v (before %+v)", m1, base)
		}
		clk.Advance(time.Second)
		release(t, st, c, store.JobQueued)
		m2 := changeMarks(t, st, "u1")
		if m2.MessagesAt == m1.MessagesAt || m2.JobsAt != clk.Now().UnixMilli() {
			t.Errorf("after the release = %+v (before %+v)", m2, m1)
		}
		if got := changeMarks(t, st, "u2"); got != other {
			t.Errorf("another user's marks moved: %+v -> %+v", other, got)
		}

		// a proactive job's partial moves the jobs mark only
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u1", c.Job.ID) })
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.InsertJob(store.Job{UserID: "u1", TriggerKind: store.TriggerSchedule, Goal: "review"})
			return err
		})
		pc := mustClaim(t, st, "w1")
		m3 := changeMarks(t, st, "u1")
		clk.Advance(time.Second)
		mustPartial(t, st, pc, store.PartialWriting, "b")
		m4 := changeMarks(t, st, "u1")
		if m4.MessagesAt != m3.MessagesAt || m4.JobsAt != clk.Now().UnixMilli() {
			t.Errorf("proactive partial = %+v (before %+v)", m4, m3)
		}
	})
}
