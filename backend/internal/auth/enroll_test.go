package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestEnrollCreatesTheTokenAndTheMemoryFolder(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	rec := e.token(t, userID)
	if rec.Status != store.TokenValid || !strings.HasPrefix(rec.Label, e.svc.LabelPrefix()) {
		t.Fatalf("token record = %+v", rec)
	}
	if got := rec.ExpiresAt.Sub(rec.CreatedAt); got != defaultTokenTTL {
		t.Fatalf("lifetime = %s, want the default %s", got, defaultTokenTTL)
	}
	live := e.fake.Tokens()
	if len(live) != 1 || live[0].Label != rec.Label || live[0].User != "einstein" {
		t.Fatalf("remote tokens = %+v", live)
	}
	cr, err := e.svc.AppCreds(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	if me, err := e.client.Me(bg, cr); err != nil || me.ID != userID {
		t.Fatalf("stored credentials must work: %+v %v", me, err)
	}
	if _, _, ok := e.fake.File(drive, "/.ocis-agent/README.md"); !ok {
		t.Fatal("the memory folder was not created")
	}
	labels := e.labels(t, userID)
	if len(labels) != 1 || labels[0].Label != rec.Label || labels[0].DeletedAt != nil {
		t.Fatalf("labels = %+v", labels)
	}
	var u store.User
	_ = e.st.View(bg, func(tx store.Tx) error { u, _ = tx.GetUser(userID); return nil })
	if u.Name != "einstein" || u.PersonalDriveID != drive {
		t.Fatalf("user = %+v (Name must be the login name)", u)
	}
	evs := e.events(t, userID)
	if len(evs) != 1 || evs[0].Kind != "token_created" || evs[0].Cred != "oidc" || evs[0].Detail != rec.Label {
		t.Fatalf("audit = %+v", evs)
	}
	e.noLeaks(t, userID, live[0].Value, e.bearer.Reveal())
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("a fresh enrolment must not be paused: %+v", p)
	}
}

func TestEnrollRejectsIncompleteInputBeforeAnyRequest(t *testing.T) {
	e := newEnv(t)
	for _, id := range []Identity{{}, {UserID: userID, Username: "einstein"}, {UserID: userID, DriveID: drive}, {Username: "einstein", DriveID: drive}} {
		if err := e.svc.Enroll(bg, id, e.bearer); err == nil {
			t.Errorf("Enroll(%+v) should fail", id)
		}
	}
	if err := e.svc.Enroll(bg, e.ident, ""); err == nil {
		t.Error("Enroll without a bearer should fail")
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d request(s) sent for invalid input", n)
	}
}

func TestFailedFirstEnrolmentLeavesNothingBehind(t *testing.T) {
	cases := map[string]func(e env){
		"verification fails": func(e env) { e.fake.Fail("GET", "/graph/v1.0/me", 401, 1) },
		"memory fails":       func(e env) { e.fake.Fail("MKCOL", "/remote.php/", 500, 1) },
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			inject(e)
			err := e.svc.Enroll(bg, e.ident, e.bearer)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), e.bearer.Reveal()) {
				t.Fatalf("the error leaks the bearer: %v", err)
			}
			if left := e.fake.Tokens(); len(left) != 0 {
				t.Fatalf("a token was left behind on oCIS: %+v", left)
			}
			if err := e.st.View(bg, func(tx store.Tx) error {
				_, err := tx.GetUser(userID)
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("user rows must be removed after a failed first enrolment, got %v", err)
			}
		})
	}
}

// An uncertain create failure (503) that finds no token must NOT close the
// label: the request may still land, and Sweep's grace path handles it.
func TestUncertainCreateFailureKeepsTheLabelOpenAndTheUser(t *testing.T) {
	e := newEnv(t)
	e.fake.Fail("POST", "/auth-app/", 503, 1)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	if left := e.fake.Tokens(); len(left) != 0 {
		t.Fatalf("a token was left behind on oCIS: %+v", left)
	}
	if err := e.st.View(bg, func(tx store.Tx) error {
		_, err := tx.GetUser(userID)
		return err
	}); err != nil {
		t.Fatalf("the user row must be kept, got %v", err)
	}
	labels := e.labels(t, userID)
	if len(labels) != 1 || labels[0].DeletedAt != nil {
		t.Fatalf("the label must stay open: %+v", labels)
	}
}

func TestATokenLandingAfterAnUncertainReEnrolmentFailureIsDeletedBySweep(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	e.fake.Fail("POST", "/auth-app/", 503, 1)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	var open string
	for _, l := range e.labels(t, userID) {
		if l.DeletedAt == nil && l.Label != e.token(t, userID).Label {
			open = l.Label
		}
	}
	if open == "" {
		t.Fatal("the attempted label must stay open")
	}
	// the in-flight create lands late
	if _, err := e.client.CreateToken(bg, ocisclient.Bearer(e.bearer), time.Hour, open); err != nil {
		t.Fatal(err)
	}
	e.fake.Advance(time.Minute)
	res, err := e.svc.Sweep(bg, userID)
	if err != nil || res.Deleted != 1 || res.Unknown != 0 || !res.Clean {
		t.Fatalf("sweep = %+v %v", res, err)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("a late landing token is not an anomaly: %+v", p)
	}
	if n := len(e.fake.Tokens()); n != 1 {
		t.Fatalf("remote tokens = %d, want only the current one", n)
	}
}

func TestFailedReEnrolmentKeepsTheWorkingToken(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	before := e.token(t, userID)
	e.fake.Fail("POST", "/auth-app/", 503, 1)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	after := e.token(t, userID)
	if after.Label != before.Label || after.Status != store.TokenValid {
		t.Fatalf("the working token was replaced: %+v -> %+v", before, after)
	}
	if _, err := e.svc.AppCreds(bg, userID); err != nil {
		t.Fatalf("credentials must still work: %v", err)
	}
	labels := e.labels(t, userID)
	if len(labels) != 2 {
		t.Fatalf("labels = %+v", labels)
	}
	for _, l := range labels {
		if l.DeletedAt != nil {
			t.Fatalf("the uncertain attempt's label must stay open: %+v", labels)
		}
	}
}

func TestReEnrolReplacesTheTokenAndClearsOnlyCredentialPauses(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	first := e.token(t, userID).Label
	e.deleteRemote(t, e.token(t, userID).Label)
	if err := e.svc.HandleUnauthorized(bg, userID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	rec := e.token(t, userID)
	if rec.Label == first || rec.Status != store.TokenValid {
		t.Fatalf("re-enrolment must store a new valid token: %+v", rec)
	}
	if p := e.policy(t, userID); p.Paused {
		t.Fatalf("the credential pause must clear: %+v", p)
	}
	if n := len(e.fake.Tokens()); n != 1 { // the old one was killed to simulate the dead credential
		t.Fatalf("only the new remote token may exist: %d tokens", n)
	}
	labels := e.labels(t, userID)
	if len(labels) != 2 || labels[0].DeletedAt != nil || labels[1].DeletedAt != nil {
		t.Fatalf("the old label stays recorded and un-deleted: %+v", labels)
	}

	// the user's own pause and a token anomaly survive re-enrolment
	for _, reason := range []string{store.PauseUser, store.PauseTokenAnomaly} {
		if err := e.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.ClearPause(userID, store.PauseUser, store.PauseTokenAnomaly)
			if err != nil {
				return err
			}
			_, err = tx.PausePolicy(userID, reason)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
			t.Fatal(err)
		}
		if p := e.policy(t, userID); p.PauseReason != reason {
			t.Fatalf("re-enrolment cleared the %q pause: %+v", reason, p)
		}
	}
}

func TestDormancyDaysApplyToNewUsersOnly(t *testing.T) {
	e := newEnv(t)
	svc, err := New(bg, Deps{Store: e.st, Secrets: e.sec, Client: e.client, Memory: e.mem, DormancyDays: 7, Now: e.fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	if got := e.policy(t, userID).DormancyDays; got != 7 {
		t.Fatalf("dormancy days = %d, want the configured 7", got)
	}
	if err := e.st.InTx(bg, func(tx store.Tx) error {
		p, _ := tx.GetPolicy(userID)
		p.DormancyDays = 3
		return tx.PutPolicy(p)
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	if got := e.policy(t, userID).DormancyDays; got != 3 {
		t.Fatalf("re-enrolment overwrote the user's setting: %d", got)
	}
}

func TestEnrolmentRejectsABearerOCISRefuses(t *testing.T) {
	e := newEnv(t)
	err := e.svc.Enroll(bg, e.ident, ocisclient.Secret("oidc:nobody"))
	if !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized in the chain", err)
	}
	if len(e.fake.Tokens()) != 0 {
		t.Fatal("no token may exist")
	}
}

func TestConcurrentEnrolmentsAreSerialised(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.svc.Enroll(bg, e.ident, e.bearer)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rec := e.token(t, userID)
	if rec.Status != store.TokenValid {
		t.Fatalf("token = %+v", rec)
	}
	if got := len(e.labels(t, userID)); got != 4 {
		t.Fatalf("labels = %d, want one per enrolment", got)
	}
	if got := len(e.fake.Tokens()); got != 4 {
		t.Fatalf("remote tokens = %d", got)
	}
}

// lostResponseSvc returns a Service over the same store as e whose client
// reaches oCIS through a proxy that lets POST /auth-app/tokens through (the
// token IS created) but replaces its response with an empty 502.
func lostResponseSvc(t *testing.T, e env) *Service {
	t.Helper()
	target, err := url.Parse(e.fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodPost && strings.HasPrefix(r.Request.URL.Path, "/auth-app/tokens") {
			r.Body.Close()
			r.StatusCode, r.Status = http.StatusBadGateway, "502 Bad Gateway"
			r.Body = http.NoBody
			r.ContentLength = 0
			r.Header.Del("Content-Length")
		}
		return nil
	}
	px := httptest.NewServer(rp)
	t.Cleanup(px.Close)
	client, err := ocisclient.New(px.URL)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(bg, Deps{Store: e.st, Secrets: e.sec, Client: client, Memory: e.mem, Now: e.fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestLostCreateResponseOnFirstEnrolmentDeletesTheToken(t *testing.T) {
	e := newEnv(t)
	if err := lostResponseSvc(t, e).Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	if left := e.fake.Tokens(); len(left) != 0 {
		t.Fatalf("a token was left behind on oCIS: %+v", left)
	}
	if err := e.st.View(bg, func(tx store.Tx) error {
		_, err := tx.GetUser(userID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user rows must be removed, got %v", err)
	}
	if evs := e.events(t, userID); len(evs) != 0 {
		t.Fatalf("audit leftovers: %+v", evs)
	}
}

func TestLostCreateResponseOnReEnrolmentDeletesTheTokenAndMarksTheLabel(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Enroll(bg, e.ident, e.bearer); err != nil {
		t.Fatal(err)
	}
	before := e.token(t, userID)
	if err := lostResponseSvc(t, e).Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	if after := e.token(t, userID); after.Label != before.Label || after.Status != store.TokenValid {
		t.Fatalf("the working token was replaced: %+v -> %+v", before, after)
	}
	if live := e.fake.Tokens(); len(live) != 1 || live[0].Label != before.Label {
		t.Fatalf("remote tokens = %+v, want only the original", live)
	}
	labels := e.labels(t, userID)
	if len(labels) != 2 {
		t.Fatalf("labels = %+v", labels)
	}
	for _, l := range labels {
		if (l.Label == before.Label) != (l.DeletedAt == nil) {
			t.Fatalf("only the abandoned label may be marked deleted: %+v", labels)
		}
	}
}

// brokenLookupStore makes GetAppToken fail with a non-NotFound error inside
// every write transaction.
type brokenLookupStore struct{ store.Store }

type brokenLookupTx struct{ store.Tx }

func (brokenLookupTx) GetAppToken(string) (store.AppToken, error) {
	return store.AppToken{}, errors.New("boom")
}

func (b brokenLookupStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return b.Store.InTx(ctx, func(tx store.Tx) error { return fn(brokenLookupTx{tx}) })
}

func TestFirstEnrolmentCleanupKeepsTheUserWhenTheTokenLookupFails(t *testing.T) {
	e := newEnv(t)
	svc, err := New(bg, Deps{Store: brokenLookupStore{e.st}, Secrets: e.sec, Client: e.client, Memory: e.mem, Now: e.fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Fail("POST", "/auth-app/", 503, 1)
	if err := svc.Enroll(bg, e.ident, e.bearer); err == nil {
		t.Fatal("expected an error")
	}
	if err := e.st.View(bg, func(tx store.Tx) error {
		_, err := tx.GetUser(userID)
		return err
	}); err != nil {
		t.Fatalf("the user rows must remain when the lookup fails with a non-NotFound error, got %v", err)
	}
}
