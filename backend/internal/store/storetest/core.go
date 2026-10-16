package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Core", runCore) }

func mustUser(t *testing.T, st store.Store, id string) store.User {
	t.Helper()
	u := store.User{ID: id, Name: "name-" + id, PersonalDriveID: "drive-" + id}
	mustTx(t, st, func(tx store.Tx) error { return tx.PutUser(u) })
	return u
}

func getPolicy(t *testing.T, st store.Store, id string) store.Policy {
	t.Helper()
	var p store.Policy
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		p, err = tx.GetPolicy(id)
		return err
	})
	if err != nil {
		t.Fatalf("GetPolicy(%s): %v", id, err)
	}
	return p
}

func runCore(t *testing.T, f Factory) {
	t.Run("enrolling a user creates the default policy", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		var u store.User
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			u, err = tx.GetUser("u1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if u.Name != "name-u1" || u.PersonalDriveID != "drive-u1" || !u.EnrolledAt.Equal(clk.Now()) || !u.LastSeenAt.Equal(clk.Now()) {
			t.Errorf("user = %+v", u)
		}
		p := getPolicy(t, st, "u1")
		want := store.DefaultPolicy("u1", clk.Now())
		if p.Paused || p.PauseReason != "" || p.AutoApproveWrite || p.ProactiveEnabled ||
			p.SweepInterval != 6*time.Hour || p.DailyTokens != 200000 || p.DormancyDays != 30 ||
			p.Watch == nil || len(p.Watch) != 0 || !p.UpdatedAt.Equal(want.UpdatedAt) {
			t.Errorf("default policy = %+v", p)
		}
	})

	t.Run("users need an id, a name and a drive", func(t *testing.T) {
		st, _ := open(t, f)
		for _, u := range []store.User{{Name: "n", PersonalDriveID: "d"}, {ID: "u", PersonalDriveID: "d"}, {ID: "u", Name: "n"}} {
			if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutUser(u) }); err == nil {
				t.Errorf("PutUser(%+v) succeeded", u)
			}
		}
	})

	t.Run("enrolling again keeps the policy and the enrolment time", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		p := getPolicy(t, st, "u1")
		p.ProactiveEnabled = true
		mustTx(t, st, func(tx store.Tx) error { return tx.PutPolicy(p) })

		clk.Advance(time.Hour)
		again := store.User{ID: "u1", Name: "renamed", PersonalDriveID: "drive-new"}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutUser(again) })

		var u store.User
		_ = st.View(bg(), func(tx store.Tx) error {
			var err error
			u, err = tx.GetUser("u1")
			return err
		})
		if u.Name != "renamed" || u.PersonalDriveID != "drive-new" {
			t.Errorf("name/drive not updated: %+v", u)
		}
		if !u.EnrolledAt.Equal(clk.Now().Add(-time.Hour)) {
			t.Errorf("EnrolledAt changed: %v", u.EnrolledAt)
		}
		if !getPolicy(t, st, "u1").ProactiveEnabled {
			t.Error("the policy was reset by re-enrolling")
		}
	})

	t.Run("unknown user is not found", func(t *testing.T) {
		st, _ := open(t, f)
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetUser("nope")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUser err = %v", err)
		}
		err = st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetPolicy("nope")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetPolicy err = %v", err)
		}
	})

	t.Run("TouchUser only moves forward", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		later := clk.Now().Add(2 * time.Hour)
		mustTx(t, st, func(tx store.Tx) error { return tx.TouchUser("u1", later) })
		mustTx(t, st, func(tx store.Tx) error { return tx.TouchUser("u1", clk.Now().Add(time.Hour)) })
		var u store.User
		_ = st.View(bg(), func(tx store.Tx) error {
			var err error
			u, err = tx.GetUser("u1")
			return err
		})
		if !u.LastSeenAt.Equal(later) {
			t.Fatalf("LastSeenAt = %v, want %v", u.LastSeenAt, later)
		}
		err := st.InTx(bg(), func(tx store.Tx) error { return tx.TouchUser("nope", later) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("TouchUser(unknown) err = %v", err)
		}
	})

	t.Run("policy round trip and validation", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		clk.Advance(time.Minute)
		p := getPolicy(t, st, "u1")
		p.Paused, p.PauseReason = true, store.PauseDormant
		p.ProactiveEnabled = true
		p.Watch = []string{"/Projects", "/Inbox"}
		p.QuietHours = "22:00-07:00"
		p.SweepInterval = 2 * time.Hour
		p.DailyTokens = 5000
		p.DormancyDays = 14
		mustTx(t, st, func(tx store.Tx) error { return tx.PutPolicy(p) })
		got := getPolicy(t, st, "u1")
		if !got.Paused || got.PauseReason != store.PauseDormant || !got.ProactiveEnabled ||
			len(got.Watch) != 2 || got.Watch[1] != "/Inbox" || got.QuietHours != "22:00-07:00" ||
			got.SweepInterval != 2*time.Hour || got.DailyTokens != 5000 || got.DormancyDays != 14 {
			t.Errorf("policy = %+v", got)
		}
		if !got.UpdatedAt.Equal(clk.Now()) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, clk.Now())
		}

		bad := map[string]func(store.Policy) store.Policy{
			"paused without a reason": func(p store.Policy) store.Policy { p.Paused, p.PauseReason = true, ""; return p },
			"reason without pause":    func(p store.Policy) store.Policy { p.Paused, p.PauseReason = false, store.PauseUser; return p },
			"unknown reason":          func(p store.Policy) store.Policy { p.Paused, p.PauseReason = true, "because"; return p },
			"non-positive sweep":      func(p store.Policy) store.Policy { p.SweepInterval = 0; return p },
			"sub-second sweep":        func(p store.Policy) store.Policy { p.SweepInterval = 500 * time.Millisecond; return p },
			"fractional sweep":        func(p store.Policy) store.Policy { p.SweepInterval = 1500 * time.Millisecond; return p },
			"negative tokens":         func(p store.Policy) store.Policy { p.DailyTokens = -1; return p },
			"negative dormancy":       func(p store.Policy) store.Policy { p.DormancyDays = -1; return p },
		}
		for name, mut := range bad {
			cand := mut(getPolicy(t, st, "u1"))
			if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutPolicy(cand) }); err == nil {
				t.Errorf("%s: PutPolicy succeeded", name)
			}
		}

		ghost := store.DefaultPolicy("ghost", clk.Now())
		err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutPolicy(ghost) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("PutPolicy for an unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("model preferences", func(t *testing.T) {
		st, _ := open(t, f)
		mustUser(t, st, "u1")
		p := getPolicy(t, st, "u1")
		if p.Models == nil || len(p.Models) != 0 {
			t.Fatalf("default Models = %#v, want empty and non-nil", p.Models)
		}

		p.Models = map[string]string{store.ModelTaskChat: "gpt-x", store.ModelTaskProactive: "meta-llama/Llama-3.1:8b"}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutPolicy(p) })
		got := getPolicy(t, st, "u1")
		if len(got.Models) != 2 || got.Models[store.ModelTaskChat] != "gpt-x" || got.Models[store.ModelTaskProactive] != "meta-llama/Llama-3.1:8b" {
			t.Fatalf("Models = %#v", got.Models)
		}

		got.ProactiveEnabled = true
		mustTx(t, st, func(tx store.Tx) error { return tx.PutPolicy(got) })
		if again := getPolicy(t, st, "u1"); len(again.Models) != 2 {
			t.Errorf("an unrelated policy update lost the preferences: %#v", again.Models)
		}

		mustUser(t, st, "u1")
		if again := getPolicy(t, st, "u1"); len(again.Models) != 2 {
			t.Errorf("enrolling again lost the preferences: %#v", again.Models)
		}

		bad := map[string]map[string]string{
			"unknown task": {"sweep": "gpt-x"},
			"empty id":     {store.ModelTaskChat: ""},
			"bad id":       {store.ModelTaskChat: "a b"},
		}
		for label, m := range bad {
			cand := getPolicy(t, st, "u1")
			cand.Models = m
			err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutPolicy(cand) })
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s: PutPolicy err = %v, want ErrInvalid", label, err)
			}
		}
		if still := getPolicy(t, st, "u1"); len(still.Models) != 2 {
			t.Errorf("a refused update changed the stored preferences: %#v", still.Models)
		}

		clear := getPolicy(t, st, "u1")
		clear.Models = nil
		mustTx(t, st, func(tx store.Tx) error { return tx.PutPolicy(clear) })
		if cleared := getPolicy(t, st, "u1"); cleared.Models == nil || len(cleared.Models) != 0 {
			t.Errorf("clearing: Models = %#v, want empty and non-nil", cleared.Models)
		}
	})

	t.Run("app token upsert, status and lookup", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		dek := mustDEK(t, st)
		tok := store.AppToken{
			UserID:    "u1",
			Sealed:    store.Sealed{DEKID: dek.ID, Nonce: []byte("n1"), Ciphertext: []byte("c1")},
			Label:     "ocis-agent-x-1",
			CreatedAt: clk.Now(),
			ExpiresAt: clk.Now().Add(72 * time.Hour),
			Status:    store.TokenValid,
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutAppToken(tok) })

		tok2 := tok
		tok2.Sealed.Nonce, tok2.Sealed.Ciphertext = []byte("n2"), []byte("c2")
		tok2.Label = "ocis-agent-x-2"
		mustTx(t, st, func(tx store.Tx) error { return tx.PutAppToken(tok2) })
		mustTx(t, st, func(tx store.Tx) error { return tx.SetAppTokenStatus("u1", store.TokenInvalid) })

		var got store.AppToken
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			got, err = tx.GetAppToken("u1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got.Label != "ocis-agent-x-2" || string(got.Sealed.Nonce) != "n2" || string(got.Sealed.Ciphertext) != "c2" ||
			got.Sealed.DEKID != dek.ID || got.Status != store.TokenInvalid ||
			!got.ExpiresAt.Equal(tok.ExpiresAt) || !got.CreatedAt.Equal(tok.CreatedAt) {
			t.Errorf("token = %+v", got)
		}

		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetAppToken("other")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetAppToken(unknown) err = %v", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { return tx.SetAppTokenStatus("other", store.TokenRevoked) })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SetAppTokenStatus(unknown) err = %v", err)
		}
	})

	t.Run("app token validation", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		dek := mustDEK(t, st)
		good := store.AppToken{
			UserID: "u1", Label: "l", CreatedAt: clk.Now(), ExpiresAt: clk.Now().Add(time.Hour), Status: store.TokenValid,
			Sealed: store.Sealed{DEKID: dek.ID, Nonce: []byte("n"), Ciphertext: []byte("c")},
		}
		bad := map[string]func(store.AppToken) store.AppToken{
			"no label":      func(a store.AppToken) store.AppToken { a.Label = ""; return a },
			"no ciphertext": func(a store.AppToken) store.AppToken { a.Sealed.Ciphertext = nil; return a },
			"no nonce":      func(a store.AppToken) store.AppToken { a.Sealed.Nonce = nil; return a },
			"bad status":    func(a store.AppToken) store.AppToken { a.Status = "weird"; return a },
			"no expiry":     func(a store.AppToken) store.AppToken { a.ExpiresAt = time.Time{}; return a },
		}
		for name, mut := range bad {
			cand := mut(good)
			if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutAppToken(cand) }); err == nil {
				t.Errorf("%s: PutAppToken succeeded", name)
			}
		}
		noCreated := good
		noCreated.CreatedAt = time.Time{}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutAppToken(noCreated) })
		var read store.AppToken
		if err := st.View(bg(), func(tx store.Tx) error {
			var err error
			read, err = tx.GetAppToken("u1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !read.CreatedAt.Equal(clk.Now()) {
			t.Errorf("CreatedAt = %v, want store clock %v", read.CreatedAt, clk.Now())
		}
		unknownDEK := good
		unknownDEK.Sealed.DEKID = "no-such-dek"
		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutAppToken(unknownDEK) }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown DEK: err = %v, want ErrNotFound", err)
		}
		unknownUser := good
		unknownUser.UserID = "ghost"
		if err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutAppToken(unknownUser) }); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown user: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("token labels", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		mustTx(t, st, func(tx store.Tx) error { return tx.AddTokenLabel("u1", "ocis-agent-x-1") })
		clk.Advance(time.Minute)
		mustTx(t, st, func(tx store.Tx) error { return tx.AddTokenLabel("u1", "ocis-agent-x-2") })

		err := st.InTx(bg(), func(tx store.Tx) error { return tx.AddTokenLabel("u1", "ocis-agent-x-1") })
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate label: err = %v, want ErrConflict", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { return tx.AddTokenLabel("ghost", "l") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("label for an unknown user: err = %v, want ErrNotFound", err)
		}

		mustTx(t, st, func(tx store.Tx) error { return tx.MarkTokenLabelDeleted("u1", "ocis-agent-x-1") })
		mustTx(t, st, func(tx store.Tx) error { return tx.MarkTokenLabelDeleted("u1", "ocis-agent-x-1") }) // idempotent
		err = st.InTx(bg(), func(tx store.Tx) error { return tx.MarkTokenLabelDeleted("u1", "unknown") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown label: err = %v, want ErrNotFound", err)
		}

		var labels []store.TokenLabel
		_ = st.View(bg(), func(tx store.Tx) error {
			var err error
			labels, err = tx.ListTokenLabels("u1")
			return err
		})
		if len(labels) != 2 || labels[0].Label != "ocis-agent-x-1" || labels[1].Label != "ocis-agent-x-2" {
			t.Fatalf("labels = %+v", labels)
		}
		if labels[0].DeletedAt == nil || labels[1].DeletedAt != nil {
			t.Errorf("deleted markers wrong: %+v", labels)
		}
	})

	t.Run("deleting a user cascades but keeps the audit log", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u1")
		dek := mustDEK(t, st)
		mustTx(t, st, func(tx store.Tx) error {
			if err := tx.PutAppToken(store.AppToken{
				UserID: "u1", Label: "l", CreatedAt: clk.Now(), ExpiresAt: clk.Now().Add(time.Hour), Status: store.TokenValid,
				Sealed: store.Sealed{DEKID: dek.ID, Nonce: []byte("n"), Ciphertext: []byte("c")},
			}); err != nil {
				return err
			}
			if err := tx.AddTokenLabel("u1", "l"); err != nil {
				return err
			}
			_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: "unenrolled"})
			return err
		})
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("u1") })

		_ = st.View(bg(), func(tx store.Tx) error {
			if _, err := tx.GetUser("u1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetUser after delete: %v", err)
			}
			if _, err := tx.GetPolicy("u1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetPolicy after delete: %v", err)
			}
			if _, err := tx.GetAppToken("u1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetAppToken after delete: %v", err)
			}
			if labels, _ := tx.ListTokenLabels("u1"); len(labels) != 0 {
				t.Errorf("labels survived: %+v", labels)
			}
			recs, err := tx.ReadAudit("u1", 0, 10)
			if err != nil || len(recs) != 1 {
				t.Errorf("audit after delete = %+v, %v", recs, err)
			}
			if err := tx.VerifyAudit("u1"); err != nil {
				t.Errorf("VerifyAudit after delete: %v", err)
			}
			return nil
		})
		err := st.InTx(bg(), func(tx store.Tx) error { return tx.DeleteUser("u1") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("deleting twice: err = %v, want ErrNotFound", err)
		}
	})
}
