package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestNewPersistsOneInstanceIDAndLabelsAreUnique(t *testing.T) {
	e := newEnv(t)
	id := e.svc.InstanceID()
	if len(id) < 8 {
		t.Fatalf("instance id %q is too short", id)
	}
	again, err := New(bg, Deps{Store: e.st, Secrets: e.sec, Client: e.client, Memory: e.mem})
	if err != nil {
		t.Fatal(err)
	}
	if again.InstanceID() != id {
		t.Fatalf("a restart changed the instance id: %q vs %q", again.InstanceID(), id)
	}
	if e.svc.LabelPrefix() != "ocis-agent-"+id+"-" {
		t.Fatalf("prefix = %q", e.svc.LabelPrefix())
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		l := e.svc.newLabel()
		if !strings.HasPrefix(l, e.svc.LabelPrefix()) || seen[l] {
			t.Fatalf("bad or repeated label %q", l)
		}
		seen[l] = true
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	e := newEnv(t)
	for name, d := range map[string]Deps{
		"store":   {Secrets: e.sec, Client: e.client, Memory: e.mem},
		"secrets": {Store: e.st, Client: e.client, Memory: e.mem},
		"client":  {Store: e.st, Secrets: e.sec, Memory: e.mem},
		"memory":  {Store: e.st, Secrets: e.sec, Client: e.client},
	} {
		if _, err := New(bg, d); err == nil {
			t.Errorf("New without %s should fail", name)
		}
	}
}

func TestAppCredsOpensTheStoredToken(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.AppCreds(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("no token yet = %v, want ErrCredentialInvalid", err)
	}
	label := e.seed(t, e.ident, 72*time.Hour)
	cr, err := e.svc.AppCreds(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Kind() != "app" {
		t.Fatalf("kind = %q", cr.Kind())
	}
	if me, err := e.client.Me(bg, cr); err != nil || me.ID != userID {
		t.Fatalf("the credentials must work: %+v %v", me, err)
	}
	// a wiped or invalid token never yields credentials
	e.deleteRemote(t, label)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AppCreds(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("invalid token = %v, want ErrCredentialInvalid", err)
	}
}

func TestAppCredsRefusesAWipedToken(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, time.Hour)
	if err := e.st.InTx(bg, func(tx store.Tx) error { return tx.WipeAppToken(userID) }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AppCreds(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestAppCredsFailsClosedForTheWrongUserSeal(t *testing.T) {
	// a ciphertext moved to another user's row must not open: the AAD binds it
	e := newEnv(t)
	e.fake.AddUser("marie", "pw", "7a7a", "drive2")
	marie := Identity{UserID: "7a7a", Username: "marie", DriveID: "drive2"}
	e.seed(t, e.ident, time.Hour)
	e.seed(t, marie, time.Hour)
	stolen := e.token(t, userID)
	stolen.UserID = "7a7a"
	if err := e.st.InTx(bg, func(tx store.Tx) error { return tx.PutAppToken(stolen) }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AppCreds(bg, "7a7a"); err == nil {
		t.Fatal("a ciphertext sealed for another user must not open")
	}
}

func TestHandleUnauthorizedPicksInvalidOrExpired(t *testing.T) {
	e := newEnv(t)
	label := e.seed(t, e.ident, 72*time.Hour)
	e.deleteRemote(t, label)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if e.token(t, userID).Status != store.TokenInvalid {
		t.Fatal("the token must be marked invalid")
	}
	if p := e.policy(t, userID); !p.Paused || p.PauseReason != store.PauseCredentialInvalid {
		t.Fatalf("policy = %+v", p)
	}
	if k := kinds(e.events(t, userID)); k != "token_invalid" {
		t.Fatalf("audit = %s", k)
	}
	// idempotent: a second report changes nothing and adds no event
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if k := kinds(e.events(t, userID)); k != "token_invalid" {
		t.Fatalf("audit after the second call = %s", k)
	}

	// after the expiry the reason is "expired"
	e.fake.AddUser("marie", "pw", "7a7a", "drive2")
	marie := Identity{UserID: "7a7a", Username: "marie", DriveID: "drive2"}
	e.seed(t, marie, time.Hour)
	e.fake.Advance(2 * time.Hour)
	if err := e.svc.HandleUnauthorized(bg, "7a7a"); err != nil {
		t.Fatal(err)
	}
	if p := e.policy(t, "7a7a"); p.PauseReason != store.PauseCredentialExpired {
		t.Fatalf("policy = %+v", p)
	}
	if k := kinds(e.events(t, "7a7a")); k != "token_expired" {
		t.Fatalf("audit = %s", k)
	}
	// an unknown user is not an error
	if err := e.svc.HandleUnauthorized(bg, "nobody"); err != nil {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestHandleUnauthorizedKeepsStrongerPauses(t *testing.T) {
	for _, reason := range []string{store.PauseUser, store.PauseTokenAnomaly} {
		e := newEnv(t)
		label := e.seed(t, e.ident, time.Hour)
		e.deleteRemote(t, label)
		if err := e.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.PausePolicy(userID, reason)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
			t.Fatal(err)
		}
		if p := e.policy(t, userID); p.PauseReason != reason {
			t.Fatalf("pause %q was replaced by %q", reason, p.PauseReason)
		}
		if e.token(t, userID).Status != store.TokenInvalid {
			t.Fatal("the token must still be marked invalid")
		}
		evs := e.events(t, userID)
		if last := evs[len(evs)-1]; last.Outcome != "kept_pause" {
			t.Fatalf("audit outcome = %q, want kept_pause", last.Outcome)
		}
	}
}

type ghostStore struct{ store.Store }
type ghostTx struct{ store.Tx }

func (ghostTx) ListDormantUsers(time.Time, int) ([]string, error) {
	return []string{"ghost", userID}, nil
}

func (g ghostTx) PausePolicy(id, reason string) (bool, error) {
	if id == "ghost" {
		return false, store.ErrNotFound
	}
	return g.Tx.PausePolicy(id, reason)
}

func (g ghostStore) View(ctx context.Context, fn func(tx store.Tx) error) error {
	return g.Store.View(ctx, func(tx store.Tx) error { return fn(ghostTx{tx}) })
}

func (g ghostStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return g.Store.InTx(ctx, func(tx store.Tx) error { return fn(ghostTx{tx}) })
}

func TestPauseDormantSkipsAUserWhosePolicyDisappeared(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, time.Hour)
	svc, err := New(bg, Deps{Store: ghostStore{e.st}, Secrets: e.sec, Client: e.client, Memory: e.mem, Now: e.fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	n, err := svc.PauseDormant(bg, 10)
	if err != nil || n != 1 {
		t.Fatalf("paused = %d, err = %v", n, err)
	}
	if p := e.policy(t, userID); p.PauseReason != store.PauseDormant {
		t.Fatalf("policy = %+v", p)
	}
}

func TestSeenClearsOnlyTheDormantPause(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, time.Hour)
	pause := func(reason string) {
		t.Helper()
		if err := e.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.PausePolicy(userID, reason)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	pause(store.PauseDormant)
	e.fake.Advance(time.Hour)
	if err := e.svc.Seen(bg, userID); err != nil {
		t.Fatal(err)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("dormant pause must clear on a visit: %+v", p)
	}
	var u store.User
	_ = e.st.View(bg, func(tx store.Tx) error { u, _ = tx.GetUser(userID); return nil })
	if !u.LastSeenAt.Equal(e.now()) {
		t.Fatalf("last seen = %v, want %v", u.LastSeenAt, e.now())
	}
	if k := kinds(e.events(t, userID)); k != "agent_resumed" {
		t.Fatalf("audit = %s", k)
	}

	pause(store.PauseUser)
	if err := e.svc.Seen(bg, userID); err != nil {
		t.Fatal(err)
	}
	if p := e.policy(t, userID); !p.Paused || p.PauseReason != store.PauseUser {
		t.Fatalf("a visit must not clear the user's own pause: %+v", p)
	}
	if err := e.svc.Seen(bg, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
}

func TestPauseDormant(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, 90*24*time.Hour)
	e.fake.AddUser("marie", "pw", "7a7a", "drive2")
	marie := Identity{UserID: "7a7a", Username: "marie", DriveID: "drive2"}
	e.seed(t, marie, 90*24*time.Hour)
	e.fake.AddUser("pierre", "pw", "8b8b", "drive3")
	pierre := Identity{UserID: "8b8b", Username: "pierre", DriveID: "drive3"}
	e.seed(t, pierre, 90*24*time.Hour)

	e.fake.Advance(25 * 24 * time.Hour)
	if err := e.svc.Seen(bg, "7a7a"); err != nil { // marie visits on day 25
		t.Fatal(err)
	}
	// pierre opted out of the dormancy pause
	if err := e.st.InTx(bg, func(tx store.Tx) error {
		p, err := tx.GetPolicy("8b8b")
		if err != nil {
			return err
		}
		p.DormancyDays = 0
		return tx.PutPolicy(p)
	}); err != nil {
		t.Fatal(err)
	}
	e.fake.Advance(10 * 24 * time.Hour) // day 35: einstein 35 days unseen, marie 10

	n, err := e.svc.PauseDormant(bg, 100)
	if err != nil || n != 1 {
		t.Fatalf("paused %d users (%v), want 1", n, err)
	}
	if p := e.policy(t, userID); p.PauseReason != store.PauseDormant {
		t.Fatalf("einstein: %+v", p)
	}
	for _, id := range []string{"7a7a", "8b8b"} {
		if e.policy(t, id).Paused {
			t.Fatalf("user %s must not be paused", id)
		}
	}
	if k := kinds(e.events(t, userID)); k != "agent_dormant" {
		t.Fatalf("audit = %s", k)
	}
	if n, err := e.svc.PauseDormant(bg, 100); err != nil || n != 0 {
		t.Fatalf("a second pass paused %d users (%v)", n, err)
	}
	// the token is NOT touched: renewal continues while dormant
	if e.token(t, userID).Status != store.TokenValid {
		t.Fatal("dormancy must not invalidate the token")
	}
}

func TestLockUserSerialisesPerUser(t *testing.T) {
	e := newEnv(t)
	unlock := e.svc.lockUser("a")
	other := e.svc.lockUser("b") // a different user is not blocked
	other()
	got := make(chan struct{})
	go func() {
		u := e.svc.lockUser("a")
		close(got)
		u()
	}()
	select {
	case <-got:
		t.Fatal("the same user's lock must be exclusive")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the lock was never released")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.svc.locks.mu.Lock()
		n := len(e.svc.locks.m)
		e.svc.locks.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d lock entries leaked", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHandleUnauthorizedIgnoresAStaleReport(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, 72*time.Hour)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if e.token(t, userID).Status != store.TokenValid {
		t.Fatal("a working token must stay valid")
	}
	if e.policy(t, userID).Paused {
		t.Fatal("a stale report must not pause")
	}
	if k := kinds(e.events(t, userID)); k != "" {
		t.Fatalf("audit = %q, want none", k)
	}
}

func TestHandleUnauthorizedIgnoresAReportAboutAReplacedToken(t *testing.T) {
	e := newEnv(t)
	oldLabel := e.seed(t, e.ident, 72*time.Hour)
	// a renewal-like swap: a newer working token replaces the stored one and the
	// old one is deleted on oCIS, so requests still holding it get 401
	label := e.svc.newLabel()
	tok, err := e.client.CreateToken(bg, ocisclient.Bearer(e.bearer), 72*time.Hour, label)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := e.sec.Seal(bg, []byte(tok.Value.Reveal()), e.svc.aad(userID))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.InTx(bg, func(tx store.Tx) error {
		if err := tx.AddTokenLabel(userID, label); err != nil {
			return err
		}
		return tx.PutAppToken(store.AppToken{
			UserID: userID, Sealed: sealed, Label: label,
			CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt, Status: store.TokenValid,
		})
	}); err != nil {
		t.Fatal(err)
	}
	e.deleteRemote(t, oldLabel)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if e.token(t, userID).Status != store.TokenValid || e.policy(t, userID).Paused {
		t.Fatal("a report about the replaced token must change nothing")
	}
	if k := kinds(e.events(t, userID)); k != "" {
		t.Fatalf("audit = %q, want none", k)
	}
}

func TestHandleUnauthorizedMarksNothingOnATransientError(t *testing.T) {
	e := newEnv(t)
	e.seed(t, e.ident, 72*time.Hour)
	e.fake.Fail("GET", "/graph/v1.0/me", 503, 1)
	if err := e.svc.HandleUnauthorized(bg, userID); err == nil {
		t.Fatal("a 503 while re-checking must be returned")
	}
	if e.token(t, userID).Status != store.TokenValid || e.policy(t, userID).Paused {
		t.Fatal("nothing may be marked on a transient error")
	}
	if k := kinds(e.events(t, userID)); k != "" {
		t.Fatalf("audit = %q, want none", k)
	}
}
