package storetest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Scheduling", runScheduling) }

func pause(t *testing.T, st store.Store, user, reason string) bool {
	t.Helper()
	var changed bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		changed, err = tx.PausePolicy(user, reason)
		return err
	})
	return changed
}

func clearPause(t *testing.T, st store.Store, user string, reasons ...string) bool {
	t.Helper()
	var cleared bool
	mustTx(t, st, func(tx store.Tx) error {
		var err error
		cleared, err = tx.ClearPause(user, reasons...)
		return err
	})
	return cleared
}

func tokenFor(user, dekID string, created, expires time.Time, status store.TokenStatus) store.AppToken {
	return store.AppToken{
		UserID:    user,
		Sealed:    store.Sealed{DEKID: dekID, Nonce: []byte("nonce"), Ciphertext: []byte("ciphertext-" + user)},
		Label:     "ocis-agent-x-" + user,
		CreatedAt: created,
		ExpiresAt: expires,
		Status:    status,
	}
}

func putToken(t *testing.T, st store.Store, a store.AppToken) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error { return tx.PutAppToken(a) })
}

func runScheduling(t *testing.T, f Factory) {
	t.Run("pause precedence", func(t *testing.T) {
		type step struct {
			reason  string
			changed bool
			want    string // pause reason afterwards
		}
		cases := []struct {
			name  string
			steps []step
		}{
			{"first pause", []step{{store.PauseDormant, true, store.PauseDormant}}},
			{"a more serious reason replaces", []step{
				{store.PauseDormant, true, store.PauseDormant},
				{store.PauseTokenAnomaly, true, store.PauseTokenAnomaly}}},
			{"a less serious reason does not replace", []step{
				{store.PauseTokenAnomaly, true, store.PauseTokenAnomaly},
				{store.PauseDormant, false, store.PauseTokenAnomaly}}},
			{"the same reason is a no-op", []step{
				{store.PauseDormant, true, store.PauseDormant},
				{store.PauseDormant, false, store.PauseDormant}}},
			{"the user's pause is sticky", []step{
				{store.PauseUser, true, store.PauseUser},
				{store.PauseTokenAnomaly, false, store.PauseUser},
				{store.PauseDormant, false, store.PauseUser},
				{store.PauseUser, false, store.PauseUser}}},
			{"a user pause overrides an automatic one", []step{
				{store.PauseCredentialRevoked, true, store.PauseCredentialRevoked},
				{store.PauseUser, true, store.PauseUser}}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				st, _ := open(t, f)
				mustUser(t, st, "u1")
				for i, s := range c.steps {
					if got := pause(t, st, "u1", s.reason); got != s.changed {
						t.Fatalf("step %d: changed = %v, want %v", i, got, s.changed)
					}
					p := getPolicy(t, st, "u1")
					if !p.Paused || p.PauseReason != s.want {
						t.Fatalf("step %d: paused=%v reason=%q, want %q", i, p.Paused, p.PauseReason, s.want)
					}
				}
			})
		}
	})

	t.Run("pause validation", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		for _, reason := range []string{"", "because"} {
			err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.PausePolicy("u1", reason); return err })
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("PausePolicy(%q): err = %v, want ErrInvalid", reason, err)
			}
		}
		err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.PausePolicy("ghost", store.PauseUser); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("clearing a pause only clears what the caller owns", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		if clearPause(t, st, "u1", store.PauseDormant) {
			t.Fatal("cleared a pause that does not exist")
		}
		pause(t, st, "u1", store.PauseTokenAnomaly)
		if clearPause(t, st, "u1", store.PauseDormant) {
			t.Fatal("cleared an anomaly pause as if it were dormancy")
		}
		if p := getPolicy(t, st, "u1"); !p.Paused || p.PauseReason != store.PauseTokenAnomaly {
			t.Fatalf("policy changed: %+v", p)
		}
		before := getPolicy(t, st, "u1").UpdatedAt
		clk.Advance(time.Minute)
		if !clearPause(t, st, "u1", store.PauseDormant, store.PauseTokenAnomaly) {
			t.Fatal("did not clear a pause that is one of the named reasons")
		}
		p := getPolicy(t, st, "u1")
		if p.Paused || p.PauseReason != "" || !p.UpdatedAt.After(before) {
			t.Fatalf("after clear: %+v", p)
		}
	})

	t.Run("a user pause is cleared only by naming the user reason", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		pause(t, st, "u1", store.PauseUser)
		before := getPolicy(t, st, "u1").UpdatedAt
		clk.Advance(time.Minute)
		auto := []string{store.PauseDormant, store.PauseCredentialExpired, store.PauseCredentialInvalid,
			store.PauseCredentialRevoked, store.PauseTokenAnomaly}
		if clearPause(t, st, "u1", auto...) {
			t.Fatal("automatic reasons cleared the user's pause")
		}
		p := getPolicy(t, st, "u1")
		if !p.Paused || p.PauseReason != store.PauseUser || !p.UpdatedAt.Equal(before) {
			t.Fatalf("a mismatched clear changed the policy: %+v (updated_at was %v)", p, before)
		}
		if !clearPause(t, st, "u1", store.PauseUser) {
			t.Fatal("did not clear the user's pause when naming it")
		}
		if p := getPolicy(t, st, "u1"); p.Paused || p.PauseReason != "" {
			t.Fatalf("after clear: %+v", p)
		}
	})

	t.Run("clear validation", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		err := st.InTx(bg(), func(tx store.Tx) error { _, err := tx.ClearPause("u1"); return err })
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no reasons: err = %v, want ErrInvalid", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { _, err := tx.ClearPause("u1", "because"); return err })
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("unknown reason: err = %v, want ErrInvalid", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { _, err := tx.ClearPause("ghost", store.PauseUser); return err })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a no-op pause does not touch updated_at", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		pause(t, st, "u1", store.PauseTokenAnomaly)
		before := getPolicy(t, st, "u1").UpdatedAt
		clk.Advance(time.Hour)
		if pause(t, st, "u1", store.PauseDormant) {
			t.Fatal("a lower-ranked pause changed the policy")
		}
		if after := getPolicy(t, st, "u1").UpdatedAt; !after.Equal(before) {
			t.Fatalf("updated_at moved from %v to %v", before, after)
		}
	})

	t.Run("wiping a token removes the sealed data and revokes it", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		dek := mustDEK(t, st)
		putToken(t, st, tokenFor("u1", dek.ID, clk.Now(), clk.Now().Add(72*time.Hour), store.TokenValid))

		mustTx(t, st, func(tx store.Tx) error { return tx.WipeAppToken("u1") })
		var got store.AppToken
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			got, err = tx.GetAppToken("u1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(got.Sealed.Ciphertext) != 0 || len(got.Sealed.Nonce) != 0 || got.Status != store.TokenRevoked ||
			got.Label != "ocis-agent-x-u1" || got.Sealed.DEKID != dek.ID {
			t.Fatalf("wiped token = %+v", got)
		}

		// enrolling again replaces the wiped row
		putToken(t, st, tokenFor("u1", dek.ID, clk.Now(), clk.Now().Add(72*time.Hour), store.TokenValid))
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			got, err = tx.GetAppToken("u1")
			return err
		}); err != nil || got.Status != store.TokenValid || len(got.Sealed.Ciphertext) == 0 {
			t.Fatalf("re-enrolled token = %+v, %v", got, err)
		}

		err := st.InTx(bg(), func(tx store.Tx) error { return tx.WipeAppToken("ghost") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("users are listed in id order with paging", func(t *testing.T) {
		st, _ := open(t, f)
		for i := 5; i >= 1; i-- {
			mustUser(t, st, fmt.Sprintf("u%d", i))
		}
		page := func(after string, limit int) []string {
			var ids []string
			if err := st.View(bg(), func(tx store.Tx) error {
				users, err := tx.ListUsers(after, limit)
				for _, u := range users {
					ids = append(ids, u.ID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return ids
		}
		if got := page("", 2); len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
			t.Errorf("first page = %v", got)
		}
		if got := page("u2", 2); len(got) != 2 || got[0] != "u3" || got[1] != "u4" {
			t.Errorf("second page = %v", got)
		}
		if got := page("u4", 2); len(got) != 1 || got[0] != "u5" {
			t.Errorf("last page = %v", got)
		}
		if got := page("u5", 2); len(got) != 0 {
			t.Errorf("past the end = %v", got)
		}
		if got := page("", 0); len(got) != 5 {
			t.Errorf("default limit = %v", got)
		}
	})

	t.Run("dormant users", func(t *testing.T) {
		st, clk := open(t, f)
		start := clk.Now()
		for _, u := range []string{"idle", "active", "paused", "disabled", "week"} {
			mustUser(t, st, u)
		}
		mustTx(t, st, func(tx store.Tx) error {
			p, err := tx.GetPolicy("disabled")
			if err != nil {
				return err
			}
			p.DormancyDays = 0
			if err := tx.PutPolicy(p); err != nil {
				return err
			}
			w, err := tx.GetPolicy("week")
			if err != nil {
				return err
			}
			w.DormancyDays = 7
			return tx.PutPolicy(w)
		})
		pause(t, st, "paused", store.PauseUser)

		dormant := func(now time.Time) []string {
			var ids []string
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				ids, err = tx.ListDormantUsers(now, 0)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return ids
		}
		day := 24 * time.Hour
		if got := dormant(start.Add(7*day - time.Millisecond)); len(got) != 0 {
			t.Errorf("one millisecond short of the week limit: %v", got)
		}
		if got := dormant(start.Add(7 * day)); len(got) != 1 || got[0] != "week" {
			t.Errorf("at 7 days: %v", got)
		}

		clk.Advance(31 * day)
		mustTx(t, st, func(tx store.Tx) error { return tx.TouchUser("active", clk.Now()) })
		got := dormant(clk.Now())
		if len(got) != 2 || got[0] != "idle" || got[1] != "week" {
			t.Errorf("after 31 days want idle and week (not active, paused or disabled), got %v", got)
		}
		if got := dormant(start.Add(30*day - time.Millisecond)); len(got) != 1 || got[0] != "week" {
			t.Errorf("just under 30 days: %v", got)
		}
		if got := dormant(start.Add(30 * day)); len(got) != 2 {
			t.Errorf("at exactly 30 days (inclusive): %v", got)
		}
	})

	t.Run("tokens are due at half of their lifetime", func(t *testing.T) {
		st, clk := open(t, f)
		for _, u := range []string{"a", "b", "c", "d"} {
			mustUser(t, st, u)
		}
		dek := mustDEK(t, st)
		t0 := clk.Now()
		putToken(t, st, tokenFor("a", dek.ID, t0, t0.Add(72*time.Hour), store.TokenValid))
		putToken(t, st, tokenFor("b", dek.ID, t0, t0.Add(24*time.Hour), store.TokenValid))
		putToken(t, st, tokenFor("c", dek.ID, t0, t0.Add(24*time.Hour), store.TokenInvalid))
		putToken(t, st, tokenFor("d", dek.ID, t0, t0.Add(10*time.Hour), store.TokenRevoked))

		due := func(now time.Time, limit int) []store.TokenDue {
			var out []store.TokenDue
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				out, err = tx.ListTokensDueForRenewal(now, limit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if got := due(t0.Add(12*time.Hour-time.Millisecond), 0); len(got) != 0 {
			t.Errorf("before the half-life: %+v", got)
		}
		got := due(t0.Add(12*time.Hour), 0)
		if len(got) != 1 || got[0].UserID != "b" || got[0].Label != "ocis-agent-x-b" || !got[0].ExpiresAt.Equal(t0.Add(24*time.Hour)) {
			t.Errorf("at b's half-life: %+v", got)
		}
		got = due(t0.Add(100*time.Hour), 0)
		if len(got) != 2 || got[0].UserID != "b" || got[1].UserID != "a" {
			t.Errorf("long after: %+v (expired tokens are due, invalid and revoked ones are not, soonest expiry first)", got)
		}
		if got := due(t0.Add(100*time.Hour), 1); len(got) != 1 || got[0].UserID != "b" {
			t.Errorf("limit 1: %+v", got)
		}
	})

	t.Run("cursors", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		get := func(user string) store.Cursor {
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
		if c := get("u1"); c.UserID != "u1" || c.SSELastEventID != "" || c.LastSweepAt != nil || c.MemoryETags == nil || len(c.MemoryETags) != 0 {
			t.Fatalf("absent cursor = %+v", c)
		}

		swept := clk.Now().Add(-time.Hour)
		mustTx(t, st, func(tx store.Tx) error {
			return tx.PutCursor(store.Cursor{
				UserID: "u1", SSELastEventID: "evt-42", LastSweepAt: &swept,
				MemoryETags: map[string]string{"memory/profile.md": `"e1"`},
			})
		})
		c := get("u1")
		if c.SSELastEventID != "evt-42" || c.LastSweepAt == nil || !c.LastSweepAt.Equal(swept) || c.MemoryETags["memory/profile.md"] != `"e1"` {
			t.Fatalf("cursor = %+v", c)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutCursor(store.Cursor{UserID: "u1", SSELastEventID: "evt-43"}) })
		if c := get("u1"); c.SSELastEventID != "evt-43" || c.LastSweepAt != nil || len(c.MemoryETags) != 0 {
			t.Fatalf("a replaced cursor kept old data: %+v", c)
		}

		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutCursor(store.Cursor{}) }); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("no user id: err = %v, want ErrInvalid", err)
		}
		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutCursor(store.Cursor{UserID: "ghost"}) }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("u1") })
		if c := get("u1"); c.SSELastEventID != "" {
			t.Errorf("a deleted user's cursor survived: %+v", c)
		}
	})
}
