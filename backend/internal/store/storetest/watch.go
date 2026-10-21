package storetest

import (
	"slices"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Watch", runWatch) }

func watchUsers(t *testing.T, st store.Store, after string, limit int) []string {
	t.Helper()
	var ids []string
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		ids, err = tx.ListWatchUsers(after, limit)
		return err
	}); err != nil {
		t.Fatalf("ListWatchUsers: %v", err)
	}
	return ids
}

func getCursor(t *testing.T, st store.Store, user string) store.Cursor {
	t.Helper()
	var c store.Cursor
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		c, err = tx.GetCursor(user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func runWatch(t *testing.T, f Factory) {
	t.Run("watch users: enabled, not paused, with watch folders, in id order", func(t *testing.T) {
		st, _ := open(t, f)
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			mustUser(t, st, id)
		}
		watch := func(p *store.Policy) { p.ProactiveEnabled, p.Watch = true, []string{"/Documents"} }
		// a, c, f: watched; b: enabled without folders; d: folders but
		// disabled; e: paused
		setProactive(t, st, "a", watch)
		setProactive(t, st, "b", func(p *store.Policy) { p.ProactiveEnabled = true })
		setProactive(t, st, "c", watch)
		setProactive(t, st, "d", func(p *store.Policy) { p.Watch = []string{"/Documents"} })
		setProactive(t, st, "e", watch)
		pause(t, st, "e", store.PauseUser)
		setProactive(t, st, "f", watch)

		if got := watchUsers(t, st, "", 0); !slices.Equal(got, []string{"a", "c", "f"}) {
			t.Fatalf("watch users = %v, want [a c f]", got)
		}
		if got := watchUsers(t, st, "a", 1); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("after a, limit 1 = %v, want [c]", got)
		}
		if got := watchUsers(t, st, "f", 0); len(got) != 0 {
			t.Fatalf("after the last = %v, want none", got)
		}
		// the watch list emptied again
		setProactive(t, st, "c", func(p *store.Policy) { p.Watch = []string{} })
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("f") })
		if got := watchUsers(t, st, "", 0); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("watch users = %v, want [a]", got)
		}
	})

	t.Run("cursor keeps the watch state", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		if c := getCursor(t, st, "u1"); c.WatchETags == nil || len(c.WatchETags) != 0 || c.WatchDirtySince != nil || c.WatchChangedAt != nil {
			t.Fatalf("absent cursor = %+v", c)
		}
		since, changed := clk.Now().Add(-time.Minute), clk.Now()
		mustTx(t, st, func(tx store.Tx) error {
			return tx.PutCursor(store.Cursor{
				UserID: "u1", WatchETags: map[string]string{"/Documents": `"e1"`, "/": "list:abc"},
				WatchDirtySince: &since, WatchChangedAt: &changed,
			})
		})
		c := getCursor(t, st, "u1")
		if c.WatchETags["/Documents"] != `"e1"` || c.WatchETags["/"] != "list:abc" || len(c.WatchETags) != 2 ||
			c.WatchDirtySince == nil || !c.WatchDirtySince.Equal(since) || c.WatchChangedAt == nil || !c.WatchChangedAt.Equal(changed) {
			t.Fatalf("cursor = %+v", c)
		}
		// a proactive start (read-modify-write of the cursor) keeps it
		setProactive(t, st, "u1", func(p *store.Policy) { p.ProactiveEnabled = true })
		mustTx(t, st, func(tx store.Tx) error {
			_, err := store.StartProactive(tx, "u1", store.TriggerSchedule, clk.Now(), "")
			return err
		})
		if c := getCursor(t, st, "u1"); c.WatchETags["/Documents"] != `"e1"` || c.WatchDirtySince == nil || c.LastSweepAt == nil {
			t.Fatalf("after a start = %+v", c)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutCursor(store.Cursor{UserID: "u1"}) })
		if c := getCursor(t, st, "u1"); len(c.WatchETags) != 0 || c.WatchETags == nil || c.WatchDirtySince != nil || c.WatchChangedAt != nil {
			t.Fatalf("a replaced cursor kept the watch state: %+v", c)
		}
	})
}
