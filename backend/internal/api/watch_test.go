package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Switching proactive work on starts the watch poll from fresh baselines: a
// change from before (or while it was off) never fires a job.
func TestEnablingProactiveResetsTheWatchState(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	since := s.fake.Now().Add(-time.Hour)
	if err := s.st.InTx(bg, func(tx store.Tx) error {
		return tx.PutCursor(store.Cursor{
			UserID: einsteinID, WatchETags: map[string]string{"/Documents": `"old"`},
			WatchDirtySince: &since, WatchChangedAt: &since,
		})
	}); err != nil {
		t.Fatal(err)
	}
	cursor := func() store.Cursor {
		var c store.Cursor
		if err := s.st.View(bg, func(tx store.Tx) error {
			var err error
			c, err = tx.GetCursor(einsteinID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(body string) {
		t.Helper()
		if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", body); rec.Code != http.StatusOK {
			t.Fatalf("put %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	put(`{"quiet_hours":"22:00-07:00"}`)
	if c := cursor(); len(c.WatchETags) != 1 || c.WatchDirtySince == nil {
		t.Fatalf("the watch state changed without an enable: %+v", c)
	}
	put(`{"proactive_enabled":true}`)
	if c := cursor(); len(c.WatchETags) != 0 || c.WatchDirtySince != nil || c.WatchChangedAt != nil {
		t.Fatalf("enabling kept the watch state: %+v", c)
	}
}

// Changing the watch list drops a pending change (it may be of a folder no
// longer watched) but keeps the baselines.
func TestChangingTheWatchListDropsThePendingChange(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	put := func(body string) {
		t.Helper()
		if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", body); rec.Code != http.StatusOK {
			t.Fatalf("put %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	put(`{"proactive_enabled":true,"watch":["/Documents"]}`)
	since := s.fake.Now()
	if err := s.st.InTx(bg, func(tx store.Tx) error {
		return tx.PutCursor(store.Cursor{
			UserID: einsteinID, WatchETags: map[string]string{"/Documents": `"e"`},
			WatchDirtySince: &since, WatchChangedAt: &since,
		})
	}); err != nil {
		t.Fatal(err)
	}
	get := func() store.Cursor {
		var c store.Cursor
		if err := s.st.View(bg, func(tx store.Tx) error {
			var err error
			c, err = tx.GetCursor(einsteinID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	put(`{"watch":["/Documents"]}`) // unchanged
	if c := get(); c.WatchDirtySince == nil {
		t.Fatal("an unchanged list dropped the pending change")
	}
	put(`{"watch":["/Documents","/Projects"]}`)
	if c := get(); c.WatchDirtySince != nil || c.WatchChangedAt != nil || c.WatchETags["/Documents"] != `"e"` {
		t.Fatalf("after a list change: %+v", c)
	}
}
