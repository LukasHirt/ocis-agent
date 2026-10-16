package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("IdempotencyOfEndedJobs", runIdempotencyOfEndedJobs) }

func listEndedIdem(t *testing.T, st store.Store, olderThan time.Time, limit int) []store.IdemRecord {
	t.Helper()
	var out []store.IdemRecord
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		out, err = tx.ListStartedIdempotentOfEndedJobs(olderThan, limit)
		return err
	})
	if err != nil {
		t.Fatalf("ListStartedIdempotentOfEndedJobs: %v", err)
	}
	return out
}

func idemKeys(recs []store.IdemRecord) []string {
	keys := make([]string, len(recs))
	for i, r := range recs {
		keys[i] = r.Key
	}
	return keys
}

func mustBegin(t *testing.T, st store.Store, user, key string) {
	t.Helper()
	if existing, err := beginKey(st, user, key); err != nil || existing != nil {
		t.Fatalf("begin %s = %+v, %v", key, existing, err)
	}
}

// endedJob claims the next queued job (it must be the one just added) and
// releases it into state.
func endedJob(t *testing.T, st store.Store, user string, state store.JobState) store.Job {
	t.Helper()
	j := addJob(t, st, user, "job "+string(state))
	c := mustClaim(t, st, "w")
	if c.Job.ID != j.ID {
		t.Fatalf("claimed %s, want %s", c.Job.ID, j.ID)
	}
	if !release(t, st, c, state) {
		t.Fatal("release not applied")
	}
	return j
}

func runIdempotencyOfEndedJobs(t *testing.T, f Factory) {
	t.Run("only started records of ended or vanished jobs are listed, oldest first", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")

		succeeded := endedJob(t, st, "u1", store.JobSucceeded)
		failed := endedJob(t, st, "u2", store.JobFailed)
		cancelled := addJob(t, st, "u1", "cancel me")
		cancelJob(t, st, "u1", cancelled.ID)
		running := addJob(t, st, "u1", "running")
		if c := mustClaim(t, st, "w"); c.Job.ID != running.ID {
			t.Fatalf("claimed %s", c.Job.ID)
		}
		waiting, _ := pendingFor(t, st, clk, "u2", "wait", 1)
		queued := addJob(t, st, "u1", "queued")

		// Started in this order, one second apart.
		keys := []struct{ user, key string }{
			{"u1", cancelled.ID + ":c1:h"},
			{"u1", running.ID + ":c1:h"},
			{"u2", waiting.Job.ID + ":c1:h"},
			{"u1", queued.ID + ":c1:h"},
			{"u2", failed.ID + ":c1:h"},
			{"u1", "ghost-job:c1:h"},       // the job no longer exists
			{"u2", succeeded.ID + ":c2:h"}, // another user's job id: not this user's job
			{"u2", running.ID + ":c4:h"},   // another user's ACTIVE job: not this user's job either
			{"u1", succeeded.ID + ":c1:h"}, // finished below
			{"u1", succeeded.ID + ":c3:h"}, // started after the cut-off
		}
		for i, k := range keys {
			if i == len(keys)-1 {
				clk.Advance(time.Hour)
			}
			mustBegin(t, st, k.user, k.key)
			clk.Advance(time.Second)
		}
		if err := finishKey(st, "u1", succeeded.ID+":c1:h", store.IdemDone, ""); err != nil {
			t.Fatal(err)
		}
		cut := clk.Now().Add(-30 * time.Minute)

		got := idemKeys(listEndedIdem(t, st, cut, 0))
		want := []string{cancelled.ID + ":c1:h", failed.ID + ":c1:h", "ghost-job:c1:h", succeeded.ID + ":c2:h", running.ID + ":c4:h"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("keys = %v, want %v", got, want)
		}
		recs := listEndedIdem(t, st, cut, 1)
		if len(recs) != 1 || recs[0].Key != want[0] || recs[0].UserID != "u1" || recs[0].State != store.IdemStarted || recs[0].Tool != "files.delete" {
			t.Fatalf("limit 1 = %+v", recs)
		}
		if all := idemKeys(listEndedIdem(t, st, clk.Now(), 0)); len(all) != 6 || all[5] != succeeded.ID+":c3:h" {
			t.Fatalf("with a later cut-off = %v", all)
		}
	})

	t.Run("records of active jobs do not hide those of ended jobs", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		waiting, _ := pendingFor(t, st, clk, "u1", "wait", 1)
		for i := 0; i < 150; i++ {
			mustBegin(t, st, "u1", fmt.Sprintf("%s:c%d:h", waiting.Job.ID, i))
		}
		clk.Advance(time.Second)
		cancelled := addJob(t, st, "u1", "cancel me")
		cancelJob(t, st, "u1", cancelled.ID)
		mustBegin(t, st, "u1", cancelled.ID+":c1:h")

		got := idemKeys(listEndedIdem(t, st, clk.Now(), 100))
		if len(got) != 1 || got[0] != cancelled.ID+":c1:h" {
			t.Fatalf("keys = %v, want only the cancelled job's record", got)
		}
	})

	t.Run("the limit defaults to 100 and is capped at 1000", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		for i := 0; i < 1001; i++ {
			mustBegin(t, st, "u1", fmt.Sprintf("gone-%04d:c:h", i))
		}
		if n := len(listEndedIdem(t, st, clk.Now(), 0)); n != 100 {
			t.Errorf("default limit = %d, want 100", n)
		}
		if n := len(listEndedIdem(t, st, clk.Now(), 5000)); n != 1000 {
			t.Errorf("capped limit = %d, want 1000", n)
		}
	})
}
