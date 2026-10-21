package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// A partial left on a job that is not running (a state no store method
// produces) is cleared by the claim and by ResumeJobIfSettled, so a run never
// starts with stale text.
func TestStalePartialIsClearedByClaimAndResume(t *testing.T) {
	s := openSQLite(t)
	ctx := context.Background()
	var queued, waiting store.Job
	if err := s.InTx(ctx, func(tx store.Tx) error {
		if err := tx.PutUser(store.User{ID: "u1", Name: "n", PersonalDriveID: "d"}); err != nil {
			return err
		}
		var err error
		if queued, err = tx.InsertJob(store.Job{UserID: "u1", TriggerKind: "chat", Goal: "q"}); err != nil {
			return err
		}
		waiting, err = tx.InsertJob(store.Job{UserID: "u1", TriggerKind: "chat", Goal: "w"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stale := func(id, state string) {
		t.Helper()
		if _, err := s.w.Exec(`UPDATE jobs SET state = ?, partial = 'stale', partial_state = 'writing', partial_at = 5 WHERE id = ?`, state, id); err != nil {
			t.Fatal(err)
		}
	}
	stale(queued.ID, "queued")
	stale(waiting.ID, "waiting_approval")

	var c store.Claim
	if err := s.InTx(ctx, func(tx store.Tx) error {
		var err error
		var ok bool
		c, ok, err = tx.ClaimNextJob("w1", time.Minute)
		if err == nil && (!ok || c.Job.ID != queued.ID) {
			t.Fatalf("claimed %v %s", ok, c.Job.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c.Job.JobPartial != (store.JobPartial{}) {
		t.Errorf("claim: %+v", c.Job.JobPartial)
	}
	if err := s.InTx(ctx, func(tx store.Tx) error {
		ok, err := tx.ResumeJobIfSettled("u1", waiting.ID)
		if err == nil && !ok {
			t.Fatal("not resumed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, func(tx store.Tx) error {
		j, err := tx.GetJob("u1", waiting.ID)
		if err == nil && j.JobPartial != (store.JobPartial{}) {
			t.Errorf("resume: %+v", j.JobPartial)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
