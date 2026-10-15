package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
)

var bg = context.Background()

func fakeClient(t *testing.T) (*fakeocis.Server, *ocisclient.Client) {
	t.Helper()
	s := fakeocis.New(t)
	s.AddUser("einstein", "relativity", "4c51", "6cb9$4c51")
	c, err := ocisclient.New(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestValidateReturnsTheIdentity(t *testing.T) {
	_, c := fakeClient(t)
	v := NewValidator(c)
	id, err := v.Validate(bg, ocisclient.Secret(fakeocis.OIDCToken("einstein")))
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{UserID: "4c51", Username: "einstein", DisplayName: "Einstein", DriveID: "6cb9$4c51"}
	if id != want {
		t.Fatalf("identity = %+v, want %+v", id, want)
	}
}

func TestValidateRejectsBadAndEmptyTokens(t *testing.T) {
	s, c := fakeClient(t)
	v := NewValidator(c)
	for _, tok := range []string{"", "nonsense", "oidc:nobody"} {
		if _, err := v.Validate(bg, ocisclient.Secret(tok)); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Validate(%q) = %v, want ErrUnauthenticated", tok, err)
		}
	}
	if n := len(s.Requests()); n == 0 {
		t.Fatal("expected the bad tokens to be checked against oCIS")
	}
	// an empty token never leaves the process
	s.ResetRequests()
	_, _ = v.Validate(bg, "")
	if len(s.Requests()) != 0 {
		t.Fatal("an empty token must not be sent")
	}
}

func TestValidatePassesServerErrorsThroughUncached(t *testing.T) {
	s, c := fakeClient(t)
	v := NewValidator(c)
	s.Fail("GET", "/graph/", 503, 1)
	_, err := v.Validate(bg, ocisclient.Secret(fakeocis.OIDCToken("einstein")))
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a 503 must not look like a bad token, got %v", err)
	}
	if _, err := v.Validate(bg, ocisclient.Secret(fakeocis.OIDCToken("einstein"))); err != nil {
		t.Fatalf("the failure must not be cached: %v", err)
	}
}

func TestValidateCachesSuccessesBriefly(t *testing.T) {
	s, c := fakeClient(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	v := NewValidator(c, WithCacheTTL(time.Minute), WithValidatorClock(func() time.Time { return now }))
	tok := ocisclient.Secret(fakeocis.OIDCToken("einstein"))
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	s.ResetRequests()
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	if len(s.Requests()) != 0 {
		t.Fatal("a cached token must not hit oCIS again")
	}
	now = now.Add(61 * time.Second)
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	if len(s.Requests()) == 0 {
		t.Fatal("an expired cache entry must be re-validated")
	}
}

func jwt(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

func TestTokenExpiryReadsAJWTAndIgnoresOtherTokens(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	got, ok := tokenExpiry(jwt(exp))
	if !ok || !got.Equal(exp) {
		t.Fatalf("tokenExpiry = %v %v", got, ok)
	}
	for _, bad := range []string{"", "opaque-token", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"x"}`)) + ".c"} {
		if _, ok := tokenExpiry(bad); ok {
			t.Errorf("tokenExpiry(%q) should be false", bad)
		}
	}
}

func TestCacheIsCappedAtTheTokenExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/graph/v1.0/me":
			_, _ = w.Write([]byte(`{"id":"u1","displayName":"U","onPremisesSamAccountName":"u"}`))
		case "/graph/v1.0/me/drives":
			_, _ = w.Write([]byte(`{"value":[{"id":"d1","driveType":"personal"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := ocisclient.New(srv.URL)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	v := NewValidator(c, WithCacheTTL(time.Hour), WithValidatorClock(func() time.Time { return now }))
	tok := ocisclient.Secret(jwt(now.Add(30 * time.Second)))
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	calls := hits.Load()
	now = now.Add(20 * time.Second)
	_, _ = v.Validate(bg, tok)
	if hits.Load() != calls {
		t.Fatal("within the token lifetime the cache must serve")
	}
	now = now.Add(15 * time.Second) // past exp, well within the one-hour cache TTL
	_, _ = v.Validate(bg, tok)
	if hits.Load() == calls {
		t.Fatal("an entry must not outlive the token's exp")
	}
}

func TestValidateRefusesAMissingLoginNameAndPersonalDrive(t *testing.T) {
	mk := func(me, drives string) *Validator {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/graph/v1.0/me" {
				_, _ = w.Write([]byte(me))
				return
			}
			_, _ = w.Write([]byte(drives))
		}))
		t.Cleanup(srv.Close)
		c, _ := ocisclient.New(srv.URL)
		return NewValidator(c)
	}
	drive := `{"value":[{"id":"d1","driveType":"personal"}]}`
	if _, err := mk(`{"id":"u1","displayName":"U"}`, drive).Validate(bg, "t"); !errors.Is(err, ErrNoLoginName) {
		t.Errorf("no login name = %v, want ErrNoLoginName", err)
	}
	if _, err := mk(`{"id":"u1","onPremisesSamAccountName":"u"}`, `{"value":[]}`).Validate(bg, "t"); !errors.Is(err, ocisclient.ErrNoPersonalDrive) {
		t.Errorf("no personal drive = %v, want ErrNoPersonalDrive", err)
	}
}

func TestCacheStaysBounded(t *testing.T) {
	_, c := fakeClient(t)
	v := NewValidator(c)
	v.maxEntries = 3
	for i := 0; i < 10; i++ {
		_, _ = v.Validate(bg, ocisclient.Secret(fakeocis.OIDCToken("einstein")+fmt.Sprint(i)))
	}
	// the suffixed tokens are invalid and never cached; valid ones are
	for i := 0; i < 10; i++ {
		v.store([32]byte{byte(i)}, Identity{UserID: "u"}, time.Now().Add(time.Hour))
	}
	if n := v.size(); n > 3 {
		t.Fatalf("cache holds %d entries, want at most 3", n)
	}
}

func TestValidateKeepsTwoUsersApart(t *testing.T) {
	s, c := fakeClient(t)
	s.AddUser("marie", "pw", "7a7a", "drive2")
	v := NewValidator(c)
	einstein := ocisclient.Secret(fakeocis.OIDCToken("einstein"))
	marie := ocisclient.Secret(fakeocis.OIDCToken("marie"))
	for i := 0; i < 2; i++ { // the second round is served from the cache
		a, err := v.Validate(bg, einstein)
		if err != nil || a.UserID != "4c51" {
			t.Fatalf("einstein = %+v %v", a, err)
		}
		b, err := v.Validate(bg, marie)
		if err != nil || b.UserID != "7a7a" || b.DriveID != "drive2" {
			t.Fatalf("marie = %+v %v", b, err)
		}
	}
	if v.size() != 2 {
		t.Fatalf("cache entries = %d", v.size())
	}
}
