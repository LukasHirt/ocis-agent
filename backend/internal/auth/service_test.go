package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

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
	e.seed(t, e.ident, 72*time.Hour)
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
	e.seed(t, e.ident, 72*time.Hour)
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
		e.seed(t, e.ident, time.Hour)
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
