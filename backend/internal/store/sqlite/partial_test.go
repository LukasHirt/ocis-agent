package sqlite

import (
	"context"
	"encoding/json"
	"strings"
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
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE jobs SET state = ? WHERE id = ?`, []any{state, id}},
			{`INSERT INTO job_partials(job_id, user_id, chat, state, text, at) VALUES(?, 'u1', 1, 'writing', 'stale', 5)`, []any{id}},
		} {
			if _, err := s.w.Exec(q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
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

// A partial write never rewrites the jobs row (whose checkpoint may be 1 MiB
// of overflow pages): it goes to the small job_partials row only.
func TestPartialWritesDoNotTouchTheJobsRow(t *testing.T) {
	s := openSQLite(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE job_updates (n INTEGER)`,
		`CREATE TRIGGER count_job_updates AFTER UPDATE ON jobs BEGIN INSERT INTO job_updates VALUES (1); END`,
	} {
		if _, err := s.w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var c store.Claim
	big := `{"pad":"` + strings.Repeat("x", 512<<10) + `"}`
	if err := s.InTx(ctx, func(tx store.Tx) error {
		if err := tx.PutUser(store.User{ID: "u1", Name: "n", PersonalDriveID: "d"}); err != nil {
			return err
		}
		if _, err := tx.InsertJob(store.Job{UserID: "u1", TriggerKind: "chat", Goal: "q"}); err != nil {
			return err
		}
		var err error
		if c, _, err = tx.ClaimNextJob("w1", time.Minute); err != nil {
			return err
		}
		return tx.SaveCheckpoint("u1", c.Job.ID, json.RawMessage(big), c.Fence)
	}); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		var n int
		if err := s.w.QueryRow(`SELECT COUNT(*) FROM job_updates`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	text := ""
	for i := range 20 {
		text += strings.Repeat("y", 300)
		state := store.PartialWriting
		if i == 19 {
			state, text = store.PartialNone, ""
		}
		if err := s.InTx(ctx, func(tx store.Tx) error {
			return tx.SetJobPartial("u1", c.Job.ID, c.Fence, state, text)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count() - before; n != 0 {
		t.Errorf("%d jobs row updates by partial writes, want 0", n)
	}
	// the change mark reads the side table alone (index only)
	rows, err := s.r.Query(`EXPLAIN QUERY PLAN `+partialMarkQuery, "u1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "COVERING INDEX job_partials_user") || strings.Contains(p, "jobs ") {
		t.Errorf("mark plan = %q", plan)
	}
}
