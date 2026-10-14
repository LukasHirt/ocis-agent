package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Idempotency", runIdempotency) }

func beginKey(st store.Store, user, key string) (*store.IdemRecord, error) {
	var existing *store.IdemRecord
	err := st.InTx(bg(), func(tx store.Tx) error {
		var err error
		existing, err = tx.BeginIdempotent(store.IdemRecord{Key: key, UserID: user, Tool: "files.delete"})
		return err
	})
	return existing, err
}

func finishKey(st store.Store, user, key string, state store.IdemState, result string) error {
	return st.InTx(bg(), func(tx store.Tx) error {
		return tx.FinishIdempotent(user, key, state, json.RawMessage(result))
	})
}

func getKey(t *testing.T, st store.Store, user, key string) store.IdemRecord {
	t.Helper()
	var r store.IdemRecord
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		r, err = tx.GetIdempotent(user, key)
		return err
	})
	if err != nil {
		t.Fatalf("GetIdempotent(%s): %v", key, err)
	}
	return r
}

func runIdempotency(t *testing.T, f Factory) {
	t.Run("begin records a started call", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		if existing, err := beginKey(st, "u1", "k1"); err != nil || existing != nil {
			t.Fatalf("first begin = %+v, %v", existing, err)
		}
		r := getKey(t, st, "u1", "k1")
		if r.State != store.IdemStarted || r.Tool != "files.delete" || !r.StartedAt.Equal(clk.Now()) || r.FinishedAt != nil || len(r.Result) != 0 {
			t.Fatalf("record = %+v", r)
		}
	})

	t.Run("a second begin returns the existing record unchanged", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		first := getKey(t, st, "u1", "k1")
		clk.Advance(time.Hour)
		existing, err := beginKey(st, "u1", "k1")
		if err != nil || existing == nil {
			t.Fatalf("second begin = %+v, %v", existing, err)
		}
		if existing.State != store.IdemStarted || !existing.StartedAt.Equal(first.StartedAt) {
			t.Fatalf("existing = %+v, want the original started record", existing)
		}
		if after := getKey(t, st, "u1", "k1"); !after.StartedAt.Equal(first.StartedAt) {
			t.Fatalf("a repeated begin rewrote StartedAt: %v", after.StartedAt)
		}
	})

	t.Run("a finished record is handed back with its result", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Minute)
		if err := finishKey(st, "u1", "k1", store.IdemDone, `{"deleted":true}`); err != nil {
			t.Fatal(err)
		}
		existing, err := beginKey(st, "u1", "k1")
		if err != nil || existing == nil || existing.State != store.IdemDone || string(existing.Result) != `{"deleted":true}` ||
			existing.FinishedAt == nil || !existing.FinishedAt.Equal(clk.Now()) {
			t.Fatalf("existing = %+v, %v", existing, err)
		}
	})

	t.Run("finish only works on a started record", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		if _, err := beginKey(st, "u1", "k2"); err != nil {
			t.Fatal(err)
		}
		if err := finishKey(st, "u1", "k1", store.IdemDone, ""); err != nil {
			t.Fatal(err)
		}
		if err := finishKey(st, "u1", "k1", store.IdemDone, ""); !errors.Is(err, store.ErrConflict) {
			t.Errorf("finishing twice: err = %v, want ErrConflict", err)
		}
		if err := finishKey(st, "u1", "k2", store.IdemFailed, `{"error":"conflict"}`); err != nil {
			t.Errorf("finish as failed: %v", err)
		}
		if err := finishKey(st, "u1", "nope", store.IdemDone, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown key: err = %v, want ErrNotFound", err)
		}
		if err := finishKey(st, "u2", "k1", store.IdemDone, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's key: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		if _, err := beginKey(st, "u1", ""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("empty key: err = %v, want ErrInvalid", err)
		}
		if _, err := beginKey(st, "", "k"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("empty user: err = %v, want ErrInvalid", err)
		}
		err := st.InTx(bg(), func(tx store.Tx) error {
			_, err := tx.BeginIdempotent(store.IdemRecord{Key: "k", UserID: "u1"})
			return err
		})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("empty tool: err = %v, want ErrInvalid", err)
		}
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		if err := finishKey(st, "u1", "k1", store.IdemStarted, ""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("finish as started: err = %v, want ErrInvalid", err)
		}
		if err := finishKey(st, "u1", "k1", store.IdemDone, `{broken`); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("broken result: err = %v, want ErrInvalid", err)
		}
		if _, err := beginKey(st, "ghost", "k9"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
		if got := getKey(t, st, "u1", "k1"); got.State != store.IdemStarted {
			t.Errorf("a refused finish changed the state to %s", got.State)
		}
	})

	t.Run("a key belongs to one user", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		mustUser(t, st, "u2")
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		if _, err := beginKey(st, "u2", "k1"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetIdempotent("u2", "k1")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another user's GetIdempotent: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("started records can be listed by age", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		start := clk.Now()
		for _, k := range []string{"old", "mid", "new", "done"} {
			if _, err := beginKey(st, "u1", k); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Minute)
		}
		if err := finishKey(st, "u1", "done", store.IdemDone, ""); err != nil {
			t.Fatal(err)
		}
		list := func(olderThan time.Time, limit int) []store.IdemRecord {
			var out []store.IdemRecord
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				out, err = tx.ListStartedIdempotent(olderThan, limit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got := list(start.Add(-time.Second), 0); len(got) != 0 {
			t.Errorf("nothing is old enough yet, got %+v", got)
		}
		if got := list(start.Add(time.Minute), 0); len(got) != 2 || got[0].Key != "old" || got[1].Key != "mid" {
			t.Errorf("olderThan boundary is inclusive and oldest first, got %+v", got)
		}
		if got := list(clk.Now(), 0); len(got) != 3 {
			t.Errorf("all started records, got %+v (finished ones must be excluded)", got)
		}
		if got := list(clk.Now(), 1); len(got) != 1 || got[0].Key != "old" {
			t.Errorf("limit 1, got %+v", got)
		}
	})

	t.Run("deleting the user removes the records", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		if _, err := beginKey(st, "u1", "k1"); err != nil {
			t.Fatal(err)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("u1") })
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetIdempotent("u1", "k1")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}
