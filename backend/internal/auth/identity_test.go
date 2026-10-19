package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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
	for _, tok := range []string{"", "nonsense", fakeocis.OIDCToken("nobody")} {
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

func TestCachedIsACacheOnlyLookup(t *testing.T) {
	s, c := fakeClient(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewValidator(c, WithCacheTTL(time.Minute), WithValidatorClock(func() time.Time { return now }))
	tok := ocisclient.Secret(fakeocis.OIDCToken("einstein"))

	s.ResetRequests()
	if _, ok := v.Cached(tok); ok {
		t.Fatal("an unvalidated token is cached")
	}
	if _, ok := v.Cached(""); ok {
		t.Fatal("the empty token is cached")
	}
	if len(s.Requests()) != 0 {
		t.Fatal("Cached called oCIS")
	}
	want, err := v.Validate(bg, tok)
	if err != nil {
		t.Fatal(err)
	}
	s.ResetRequests()
	if id, ok := v.Cached(tok); !ok || id != want {
		t.Fatalf("Cached = %+v %v", id, ok)
	}
	if len(s.Requests()) != 0 {
		t.Fatal("Cached called oCIS")
	}
	// a rejected token is never cached
	_, _ = v.Validate(bg, "nonsense")
	if _, ok := v.Cached("nonsense"); ok {
		t.Fatal("a rejected token is cached")
	}
	// an expired entry is not returned
	now = now.Add(time.Minute)
	if _, ok := v.Cached(tok); ok {
		t.Fatal("an expired entry is returned")
	}
}

func TestKnownRemembersSuccessfulValidationsOnly(t *testing.T) {
	s, c := fakeClient(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewValidator(c, WithCacheTTL(time.Minute), WithValidatorClock(func() time.Time { return now }))
	tok := ocisclient.Secret(fakeocis.OIDCToken("einstein"))

	// garbage, a rejected token and an outage never enter the set
	for _, bad := range []ocisclient.Secret{"", "nonsense", ocisclient.Secret(fakeocis.OIDCToken("nobody"))} {
		_, _ = v.Validate(bg, bad)
		if v.Known(bad) {
			t.Errorf("%q is known", bad)
		}
	}
	s.Fail("GET", "/graph/", 503, 1)
	_, _ = v.Validate(bg, tok)
	if v.Known(tok) {
		t.Fatal("a token is known after an outage")
	}
	s.ResetRequests()
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	s.ResetRequests()
	if !v.Known(tok) {
		t.Fatal("a validated token is not known")
	}
	if len(s.Requests()) != 0 {
		t.Fatal("Known called oCIS")
	}
	// it outlives the 60s cache
	now = now.Add(time.Hour)
	if _, ok := v.Cached(tok); ok || !v.Known(tok) {
		t.Fatal("known should outlive the cache")
	}
	// a later rejection removes it
	s.Fail("GET", "/graph/", 401, 1)
	if _, err := v.Validate(bg, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Validate = %v", err)
	}
	if v.Known(tok) {
		t.Fatal("a rejected token is still known")
	}
	// an outage leaves a known token known
	if _, err := v.Validate(bg, tok); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	s.Fail("GET", "/graph/", 503, 1)
	_, _ = v.Validate(bg, tok)
	if !v.Known(tok) {
		t.Fatal("an outage dropped a known token")
	}
}

func TestKnownExpires(t *testing.T) {
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
	v := NewValidator(c, WithValidatorClock(func() time.Time { return now }))
	opaque := ocisclient.Secret("opaque-token")
	short := ocisclient.Secret(jwt(now.Add(time.Hour)))
	for _, tok := range []ocisclient.Secret{opaque, short} {
		if _, err := v.Validate(bg, tok); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	if v.Known(short) {
		t.Error("known outlives the token's exp")
	}
	if !v.Known(opaque) {
		t.Error("an opaque token is forgotten before 12h")
	}
	now = now.Add(11 * time.Hour)
	if v.Known(opaque) {
		t.Error("known outlives 12h")
	}
}

func TestKnownIsBounded(t *testing.T) {
	_, c := fakeClient(t)
	v := NewValidator(c)
	v.maxKnown = 3
	now := time.Now()
	for i := range 10 {
		v.remember([32]byte{byte(i)}, fmt.Sprintf("user%d", i), now.Add(time.Hour))
	}
	if n := v.knownSize(); n != 3 {
		t.Fatalf("known holds %d entries, want 3", n)
	}
	// least recently used goes first
	v.mu.Lock()
	_, oldest := v.known[[32]byte{0}]
	_, newest := v.known[[32]byte{9}]
	v.mu.Unlock()
	if oldest || !newest {
		t.Errorf("eviction order: oldest kept %v, newest kept %v", oldest, newest)
	}
}

// One user minting many valid tokens cannot push everyone else out of the
// known set: each user keeps at most maxKnownPerUser entries.
func TestKnownKeepsTheLatestTokensPerUser(t *testing.T) {
	_, c := fakeClient(t)
	v := NewValidator(c)
	now := time.Now()
	v.remember([32]byte{0xaa}, "other", now.Add(time.Hour))
	for i := range 50 {
		v.remember([32]byte{byte(i)}, "insider", now.Add(time.Hour))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, e := range v.known {
		if e.Value.(*knownEntry).user == "insider" {
			n++
		}
	}
	if n != maxKnownPerUser {
		t.Errorf("insider holds %d entries, want %d", n, maxKnownPerUser)
	}
	if _, ok := v.known[[32]byte{0xaa}]; !ok {
		t.Error("another user's token was pushed out")
	}
	if _, ok := v.known[[32]byte{49}]; !ok {
		t.Error("the insider's newest token is missing")
	}
	if _, ok := v.known[[32]byte{0}]; ok {
		t.Error("the insider's oldest token is still known")
	}
	if v.knownPerUser["insider"] != maxKnownPerUser || v.knownPerUser["other"] != 1 {
		t.Errorf("per-user counts = %v", v.knownPerUser)
	}
}

// graphServer serves /me and /me/drives with a delay, counting the calls;
// drives answers the status in driveStatus (0: 200 with a drive).
func graphServer(t *testing.T, delay time.Duration, driveStatus *atomic.Int32) (*ocisclient.Client, *atomic.Int32, *atomic.Int32) {
	url, me, drives := graphServerURL(t, delay, driveStatus)
	c, _ := ocisclient.New(url)
	return c, me, drives
}

func graphServerURL(t *testing.T, delay time.Duration, driveStatus *atomic.Int32) (string, *atomic.Int32, *atomic.Int32) {
	var me, drives atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		switch r.URL.Path {
		case "/graph/v1.0/me":
			me.Add(1)
			_, _ = w.Write([]byte(`{"id":"u1","displayName":"U","onPremisesSamAccountName":"u"}`))
		case "/graph/v1.0/me/drives":
			drives.Add(1)
			if st := driveStatus.Load(); st != 0 {
				w.WriteHeader(int(st))
				return
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"d1","driveType":"personal"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &me, &drives
}

// Concurrent validations of one uncached token share one round trip.
func TestConcurrentValidationsOfOneTokenAreCoalesced(t *testing.T) {
	c, me, drives := graphServer(t, 100*time.Millisecond, &atomic.Int32{})
	v := NewValidator(c)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := v.Validate(bg, "the-token")
			if err == nil && id.UserID != "u1" {
				err = fmt.Errorf("identity %+v", id)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if me.Load() != 1 || drives.Load() != 1 {
		t.Errorf("/me %d, /me/drives %d calls for 20 concurrent validations, want 1 each", me.Load(), drives.Load())
	}
	// different tokens are not coalesced
	_, _ = v.Validate(bg, "another-token")
	if me.Load() != 2 {
		t.Errorf("/me calls = %d", me.Load())
	}
}

// A waiter whose own context ends stops waiting; the others still get the
// result, and a leader cancelled by its caller does not fail the waiters.
func TestCoalescedValidationRespectsEachCallersContext(t *testing.T) {
	c, me, _ := graphServer(t, 200*time.Millisecond, &atomic.Int32{})
	v := NewValidator(c)
	leaderCtx, cancelLeader := context.WithCancel(bg)
	leader := make(chan error, 1)
	go func() {
		_, err := v.Validate(leaderCtx, "tok")
		leader <- err
	}()
	time.Sleep(50 * time.Millisecond) // the leader is in flight
	waiterCtx, cancelWaiter := context.WithTimeout(bg, 20*time.Millisecond)
	defer cancelWaiter()
	if _, err := v.Validate(waiterCtx, "tok"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an impatient waiter got %v", err)
	}
	patient := make(chan error, 1)
	go func() {
		_, err := v.Validate(bg, "tok")
		patient <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-leader; !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled leader got %v", err)
	}
	if err := <-patient; err != nil {
		t.Errorf("a patient waiter failed with the leader's cancellation: %v", err)
	}
	if n := me.Load(); n > 2 {
		t.Errorf("/me calls = %d", n)
	}
}

// oCIS refusing the drive lookup of a token it accepted for /me is a
// rejection of the token (charged and forgotten), not an outage.
func TestARejectedDriveLookupIsARejection(t *testing.T) {
	for _, st := range []int32{http.StatusUnauthorized, http.StatusForbidden} {
		var drive atomic.Int32
		c, _, _ := graphServer(t, 0, &drive)
		now := time.Now()
		v := NewValidator(c, WithValidatorClock(func() time.Time { return now }))
		if _, err := v.Validate(bg, "tok"); err != nil || !v.Known("tok") {
			t.Fatalf("first validation: %v", err)
		}
		now = now.Add(2 * time.Minute) // past the cache
		drive.Store(st)
		if _, err := v.Validate(bg, "tok"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("drives %d: %v, want ErrUnauthenticated", st, err)
		}
		if v.Known("tok") {
			t.Errorf("drives %d: the token is still known", st)
		}
	}
	var drive atomic.Int32
	drive.Store(http.StatusServiceUnavailable)
	c, _, _ := graphServer(t, 0, &drive)
	if _, err := NewValidator(c).Validate(bg, "tok"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Errorf("drives 503: %v, want an outage", err)
	}
}

// panicRT panics in its next round trip once released, else forwards.
type panicRT struct {
	next    http.RoundTripper
	armed   atomic.Bool
	release chan struct{}
}

func (p *panicRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.armed.CompareAndSwap(true, false) {
		<-p.release
		panic("boom in the transport")
	}
	return p.next.RoundTrip(r)
}

// A panic in the validation leaves no flight behind: waiters get an error
// (never an empty identity without one) and the next call validates anew.
func TestAPanickingValidationLeavesNoFlight(t *testing.T) {
	var drive atomic.Int32
	base, _, _ := graphServerURL(t, 0, &drive)
	rt := &panicRT{next: http.DefaultTransport, release: make(chan struct{})}
	rt.armed.Store(true)
	c, err := ocisclient.New(base, ocisclient.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	v := NewValidator(c)
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_, _ = v.Validate(bg, "tok")
	}()
	waitFor(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.flights) == 1 })
	waiter := make(chan error, 1)
	go func() {
		id, err := v.Validate(bg, "tok")
		if err == nil && id == (Identity{}) {
			err = errors.New("an empty identity without an error")
		}
		waiter <- err
	}()
	time.Sleep(50 * time.Millisecond) // the waiter is waiting on the flight
	close(rt.release)
	if p := <-leader; p == nil {
		t.Fatal("the panic did not propagate to the leader")
	}
	if err := <-waiter; !errors.Is(err, errValidationPanicked) {
		t.Errorf("waiter = %v, want errValidationPanicked", err)
	}
	v.mu.Lock()
	n := len(v.flights)
	v.mu.Unlock()
	if n != 0 {
		t.Errorf("%d flights left", n)
	}
	if id, err := v.Validate(bg, "tok"); err != nil || id.UserID != "u1" {
		t.Errorf("after the panic: %+v %v", id, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
