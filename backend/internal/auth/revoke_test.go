package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// withForeignTokens gives the user two app tokens that are not the agent's.
func withForeignTokens(t *testing.T, e env) []string {
	t.Helper()
	labels := []string{"my-phone", "ocis-agent-someoneelse-abc"}
	for _, l := range labels {
		if _, err := e.client.CreateToken(bg, ocisclient.Bearer(e.bearer), 500*time.Hour, l); err != nil {
			t.Fatal(err)
		}
	}
	return labels
}

// withRenewalChain leaves two of the agent's own tokens on oCIS (a failed
// delete during a renewal).
func withRenewalChain(t *testing.T, e env) {
	t.Helper()
	e.fake.Advance(40 * time.Hour)
	e.fake.Fail("DELETE", "/auth-app/", 503, 1)
	if _, err := e.svc.Renew(bg, userID); err != nil {
		t.Fatal(err)
	}
	ours := 0
	for _, l := range e.remoteLabels() {
		if strings.HasPrefix(l, e.svc.LabelPrefix()) {
			ours++
		}
	}
	if ours != 2 {
		t.Fatalf("setup: %d of the agent's tokens, want a chain of 2", ours)
	}
}

func assertRevokedLocally(t *testing.T, e env) {
	t.Helper()
	rec := e.token(t, userID)
	if rec.Status != store.TokenRevoked || len(rec.Sealed.Ciphertext) != 0 {
		t.Fatalf("the stored token must be wiped: %+v", rec)
	}
	if p := e.policy(t, userID); !p.Paused || p.PauseReason != store.PauseCredentialRevoked {
		t.Fatalf("policy = %+v", p)
	}
	for _, l := range e.labels(t, userID) {
		if l.DeletedAt == nil {
			t.Fatalf("label %q still open", l.Label)
		}
	}
	if _, err := e.svc.AppCreds(bg, userID); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("AppCreds after revoke = %v", err)
	}
}

func TestRevokeDeletesEveryTokenWithOurPrefixAndNothingElse(t *testing.T) {
	e := enrolled(t)
	foreign := withForeignTokens(t, e)
	withRenewalChain(t, e)
	e.fake.ResetRequests()

	n, err := e.svc.Revoke(bg, userID, e.bearer)
	if err != nil || n != 2 {
		t.Fatalf("revoked %d (%v), want the whole chain of 2", n, err)
	}
	left := e.remoteLabels()
	if len(left) != len(foreign) {
		t.Fatalf("remote = %v, want only the foreign tokens %v", left, foreign)
	}
	for _, l := range left {
		if strings.HasPrefix(l, e.svc.LabelPrefix()) {
			t.Fatalf("a token with our prefix survived: %q", l)
		}
	}
	assertRevokedLocally(t, e)
	evs := e.events(t, userID)
	last := evs[len(evs)-1]
	if last.Kind != "token_deleted" || last.Cred != "oidc" || last.Detail != "tokens=2" {
		t.Fatalf("last audit = %+v", last)
	}
	for _, r := range e.fake.Requests() {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer") {
			t.Fatalf("revoke must use the OIDC bearer, not the app token: %s %s", r.Method, r.Path)
		}
	}
}

func TestRevokeAllNeedsConfirmationAndSendsNothingWithout(t *testing.T) {
	e := enrolled(t)
	e.fake.ResetRequests()
	if _, err := e.svc.RevokeAll(bg, userID, e.bearer, false); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("err = %v, want ErrNotConfirmed", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d request(s) without confirmation", n)
	}
	if e.token(t, userID).Status != store.TokenValid || len(e.fake.Tokens()) != 1 {
		t.Fatal("nothing may change without confirmation")
	}
}

func TestRevokeAllDeletesTheUsersOtherAppPasswordsToo(t *testing.T) {
	e := enrolled(t)
	withForeignTokens(t, e)
	n, err := e.svc.RevokeAll(bg, userID, e.bearer, true)
	if err != nil || n != 3 {
		t.Fatalf("revoked %d (%v), want 3", n, err)
	}
	if left := e.fake.Tokens(); len(left) != 0 {
		t.Fatalf("remote tokens = %+v", left)
	}
	assertRevokedLocally(t, e)
	evs := e.events(t, userID)
	last := evs[len(evs)-1]
	if last.Kind != "token_revoked_all" || last.Detail != "tokens=3" {
		t.Fatalf("last audit = %+v", last)
	}
	e.noLeaks(t, userID, "my-phone", "someoneelse")
}

func TestPreviewListsTokensWithoutHashes(t *testing.T) {
	e := enrolled(t)
	withForeignTokens(t, e)
	e.fake.ResetRequests()
	sum, err := e.svc.PreviewRevokeAll(bg, e.bearer)
	if err != nil || len(sum) != 3 {
		t.Fatalf("preview = %+v %v", sum, err)
	}
	ours := 0
	for _, s := range sum {
		if s.Label == "" || s.CreatedAt.IsZero() || s.ExpiresAt.IsZero() {
			t.Fatalf("incomplete summary %+v", s)
		}
		if s.Ours {
			ours++
			if !strings.HasPrefix(s.Label, e.svc.LabelPrefix()) {
				t.Fatalf("Ours flag on a foreign label: %+v", s)
			}
		}
		if strings.Contains(fmt.Sprintf("%+v %#v", s, s), "$2a$") {
			t.Fatalf("a hash leaked into the summary: %+v", s)
		}
	}
	if ours != 1 {
		t.Fatalf("ours = %d", ours)
	}
	for _, r := range e.fake.Requests() {
		if r.Method != "GET" {
			t.Fatalf("a preview must not change anything: %s %s", r.Method, r.Path)
		}
	}
}

func TestFailedRevokeChangesNothingLocallyAndCanBeRetried(t *testing.T) {
	e := enrolled(t)
	e.fake.Fail("DELETE", "/auth-app/", 503, 1)
	if _, err := e.svc.Revoke(bg, userID, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	if e.token(t, userID).Status != store.TokenValid || e.policy(t, userID).Paused {
		t.Fatal("a failed revoke must leave the local state alone")
	}
	if n, err := e.svc.Revoke(bg, userID, e.bearer); err != nil || n != 1 {
		t.Fatalf("retry = %d %v", n, err)
	}
	assertRevokedLocally(t, e)
}

func TestRevokeDetectsATokenThatSurvivedTheDelete(t *testing.T) {
	e := enrolled(t)
	e.fake.Fail("DELETE", "/auth-app/", 200, 1) // answers OK but deletes nothing
	if _, err := e.svc.Revoke(bg, userID, e.bearer); !errors.Is(err, ErrRevokeIncomplete) {
		t.Fatalf("err = %v, want ErrRevokeIncomplete", err)
	}
	if e.token(t, userID).Status != store.TokenValid {
		t.Fatal("nothing local may change while a token survives")
	}
}

func TestRevokeRefusesABadBearer(t *testing.T) {
	e := enrolled(t)
	for _, f := range []func() error{
		func() error { _, err := e.svc.Revoke(bg, userID, "oidc:nobody"); return err },
		func() error { _, err := e.svc.RevokeAll(bg, userID, "oidc:nobody", true); return err },
		func() error { _, err := e.svc.PreviewRevokeAll(bg, "oidc:nobody"); return err },
		func() error { return e.svc.Unenroll(bg, userID, "oidc:nobody", true) },
	} {
		if err := f(); !errors.Is(err, ocisclient.ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	}
	if e.token(t, userID).Status != store.TokenValid || len(e.fake.Tokens()) != 1 {
		t.Fatal("a refused bearer must change nothing")
	}
}

func TestARevokedTokenIsNeverResurrectedByARenewal(t *testing.T) {
	for i := 0; i < 10; i++ {
		e := enrolled(t)
		e.fake.Advance(40 * time.Hour)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = e.svc.Renew(bg, userID) }()
		go func() { defer wg.Done(); _, _ = e.svc.Revoke(bg, userID, e.bearer) }()
		wg.Wait()
		if rec := e.token(t, userID); rec.Status != store.TokenRevoked {
			t.Fatalf("round %d: status %q, the revoke must win", i, rec.Status)
		}
		for _, l := range e.remoteLabels() {
			if strings.HasPrefix(l, e.svc.LabelPrefix()) {
				t.Fatalf("round %d: token %q survived the revoke", i, l)
			}
		}
	}
}

func TestUnenrollRemovesTheUserButKeepsTheAuditLog(t *testing.T) {
	e := enrolled(t)
	if err := e.svc.Unenroll(bg, userID, e.bearer, true); err != nil {
		t.Fatal(err)
	}
	if err := e.st.View(bg, func(tx store.Tx) error {
		if _, err := tx.GetUser(userID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("user still exists: %v", err)
		}
		if _, err := tx.GetPolicy(userID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("policy still exists: %v", err)
		}
		return tx.VerifyAudit(userID)
	}); err != nil {
		t.Fatal(err)
	}
	if k := kinds(e.events(t, userID)); !strings.HasSuffix(k, "token_deleted,unenrolled") {
		t.Fatalf("audit = %s", k)
	}
	if len(e.fake.Tokens()) != 0 {
		t.Fatal("tokens left on oCIS")
	}
	if _, _, ok := e.fake.File(drive, "/.ocis-agent/README.md"); ok {
		t.Fatal("the memory folder must be deleted when asked")
	}
}

func TestUnenrollCanKeepTheMemoryFolder(t *testing.T) {
	e := enrolled(t)
	if err := e.svc.Unenroll(bg, userID, e.bearer, false); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.fake.File(drive, "/.ocis-agent/README.md"); !ok {
		t.Fatal("the memory folder must stay when not asked to delete it")
	}
}

func TestUnenrollOfAnUnknownUserIsNotFound(t *testing.T) {
	e := newEnv(t)
	e.fake.ResetRequests()
	if err := e.svc.Unenroll(bg, userID, e.bearer, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d request(s) for an unknown user", n)
	}
}

func TestFailedMemoryPurgeKeepsTheUserSoUnenrollCanBeRetried(t *testing.T) {
	e := enrolled(t)
	e.fake.Fail("DELETE", "/remote.php/", 503, 1)
	if err := e.svc.Unenroll(bg, userID, e.bearer, true); err == nil {
		t.Fatal("expected an error")
	}
	if err := e.st.View(bg, func(tx store.Tx) error { _, err := tx.GetUser(userID); return err }); err != nil {
		t.Fatalf("the user must remain after a failed unenroll: %v", err)
	}
	if err := e.svc.Unenroll(bg, userID, e.bearer, true); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestRevokeRefusesABearerThatDoesNotBelongToTheUser(t *testing.T) {
	e := enrolled(t)
	e.fake.AddUser("marie", "pw", "7a7a", "drive2")
	marie := Identity{UserID: "7a7a", Username: "marie", DriveID: "drive2"}
	e.seed(t, marie, time.Hour)
	withForeignTokens(t, e)
	before := len(e.fake.Tokens())
	// einstein's bearer, marie's user id
	for _, f := range []func() error{
		func() error { _, err := e.svc.Revoke(bg, marie.UserID, e.bearer); return err },
		func() error { _, err := e.svc.RevokeAll(bg, marie.UserID, e.bearer, true); return err },
		func() error { return e.svc.Unenroll(bg, marie.UserID, e.bearer, true) },
	} {
		if err := f(); err == nil || errors.Is(err, ocisclient.ErrUnauthorized) {
			t.Fatalf("err = %v, want a plain pairing error", err)
		}
	}
	if len(e.fake.Tokens()) != before {
		t.Fatal("a mismatched pair must delete nothing")
	}
	if e.token(t, marie.UserID).Status != store.TokenValid || e.token(t, userID).Status != store.TokenValid {
		t.Fatal("a mismatched pair must change nothing locally")
	}
	if len(e.events(t, marie.UserID)) != 0 {
		t.Fatalf("audit = %s", kinds(e.events(t, marie.UserID)))
	}
}

func TestRevokeOfAnUnenrolledUserStillWritesTheAudit(t *testing.T) {
	e := newEnv(t)
	withForeignTokens(t, e)
	n, err := e.svc.RevokeAll(bg, userID, e.bearer, true)
	if err != nil || n != 2 {
		t.Fatalf("n = %d, err = %v", n, err)
	}
	evs := e.events(t, userID)
	if len(evs) != 1 || evs[0].Kind != "token_revoked_all" || evs[0].Cred != "oidc" || evs[0].Detail != "tokens=2" {
		t.Fatalf("audit = %+v", evs)
	}
}

// A revoke that deleted nothing for a user who never enrolled leaves no
// trace: there is no state and no action to record.
func TestAnEmptyRevokeOfAnUnenrolledUserWritesNoAudit(t *testing.T) {
	e := newEnv(t)
	for _, revoke := range []func() (int, error){
		func() (int, error) { return e.svc.Revoke(bg, userID, e.bearer) },
		func() (int, error) { return e.svc.RevokeAll(bg, userID, e.bearer, true) },
	} {
		if n, err := revoke(); err != nil || n != 0 {
			t.Fatalf("n = %d, err = %v", n, err)
		}
	}
	if evs := e.events(t, userID); len(evs) != 0 {
		t.Fatalf("audit = %+v", evs)
	}
}

// cancelStore cancels the caller's context just before every transaction, as
// a request cancelled right after the remote deletes would.
type cancelStore struct {
	store.Store
	cancel *context.CancelFunc // armed after construction
}

func (c cancelStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	if *c.cancel != nil {
		(*c.cancel)()
	}
	return c.Store.InTx(ctx, fn)
}

func TestRevokeFinishesLocallyEvenIfTheRequestIsCancelledAfterTheDeletes(t *testing.T) {
	e := enrolled(t)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	var arm context.CancelFunc
	svc, err := New(bg, Deps{Store: cancelStore{e.st, &arm}, Secrets: e.sec, Client: e.client, Memory: e.mem, Now: e.fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	arm = cancel
	if _, err := svc.Revoke(ctx, userID, e.bearer); err != nil {
		t.Fatal(err)
	}
	assertRevokedLocally(t, e)
}

func TestRevokeAuditOutcomeShowsAKeptPause(t *testing.T) {
	e := enrolled(t)
	if err := e.st.InTx(bg, func(tx store.Tx) error { _, err := tx.PausePolicy(userID, store.PauseUser); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Revoke(bg, userID, e.bearer); err != nil {
		t.Fatal(err)
	}
	evs := e.events(t, userID)
	if last := evs[len(evs)-1]; last.Outcome != "kept_pause" {
		t.Fatalf("outcome = %q", last.Outcome)
	}
}
