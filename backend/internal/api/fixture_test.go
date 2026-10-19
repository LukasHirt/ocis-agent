package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

const (
	testOrigin = "https://cloud.example.com"
	goodToken  = "good-oidc-token"
	otherToken = "other-oidc-token"
)

var (
	aliceID = auth.Identity{UserID: "u-alice", Username: "alice", DisplayName: "Alice", DriveID: "d-alice"}
	bobID   = auth.Identity{UserID: "u-bob", Username: "bob", DisplayName: "Bob", DriveID: "d-bob"}
)

// fakeValidator accepts exactly the tokens it knows; err, if set, is
// returned for every call (an oCIS outage).
type fakeValidator struct {
	mu     sync.Mutex
	calls  int
	tokens map[string]auth.Identity
	err    error
	delay  time.Duration            // each Validate takes this long (an oCIS round trip)
	cached map[string]auth.Identity // what Validate accepted, like the real cache
	known  map[string]bool          // like auth.Validator.Known
}

// Validate behaves like auth.Validator: a context that ended is a context
// error, an outage (err) changes nothing, a rejection forgets the token, a
// success caches it and makes it known.
func (v *fakeValidator) Validate(ctx context.Context, tok ocisclient.Secret) (auth.Identity, error) {
	v.mu.Lock()
	v.calls++
	d := v.delay
	v.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	if err := ctx.Err(); err != nil {
		return auth.Identity{}, fmt.Errorf("ocisclient: GET /graph/v1.0/me: %w", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return auth.Identity{}, v.err
	}
	id, ok := v.tokens[tok.Reveal()]
	if !ok {
		delete(v.known, tok.Reveal())
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	if v.cached == nil {
		v.cached, v.known = map[string]auth.Identity{}, map[string]bool{}
	}
	v.cached[tok.Reveal()] = id
	v.known[tok.Reveal()] = true
	return id, nil
}

// Cached and Known never count as validator calls: no network request.
func (v *fakeValidator) Cached(tok ocisclient.Secret) (auth.Identity, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	id, ok := v.cached[tok.Reveal()]
	return id, ok
}

func (v *fakeValidator) Known(tok ocisclient.Secret) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.known[tok.Reveal()]
}

// expireCache drops the cache (its 60s TTL passed); known tokens stay.
func (v *fakeValidator) expireCache() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cached = map[string]auth.Identity{}
}

func (v *fakeValidator) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

var errNotUsed = errors.New("fake: not used in this test")

// fakeCreds records Seen calls; the other methods are not used by the
// pipeline tests.
type fakeCreds struct {
	mu      sync.Mutex
	seen    []string
	seenErr error
}

func (c *fakeCreds) Seen(_ context.Context, userID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, userID)
	return c.seenErr
}

func (c *fakeCreds) seenCalls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

func (c *fakeCreds) Enroll(context.Context, auth.Identity, ocisclient.Secret) error {
	return errNotUsed
}
func (c *fakeCreds) Unenroll(context.Context, string, ocisclient.Secret, bool) error {
	return errNotUsed
}
func (c *fakeCreds) Revoke(context.Context, string, ocisclient.Secret) (int, error) {
	return 0, errNotUsed
}
func (c *fakeCreds) RevokeAll(context.Context, string, ocisclient.Secret, bool) (int, error) {
	return 0, errNotUsed
}
func (c *fakeCreds) PreviewRevokeAll(context.Context, ocisclient.Secret) ([]auth.TokenSummary, error) {
	return nil, errNotUsed
}
func (c *fakeCreds) Renew(context.Context, string) (auth.Renewed, error) {
	return auth.Renewed{}, errNotUsed
}

type fakeDecider struct{}

func (fakeDecider) Decide(context.Context, string, string, []approvals.DecisionItem) ([]store.Approval, error) {
	return nil, errNotUsed
}

type fakeWaker struct {
	mu sync.Mutex
	n  int
}

func (w *fakeWaker) Wake() {
	w.mu.Lock()
	w.n++
	w.mu.Unlock()
}

// fakeClock is the server's clock (the rate limiter reads it).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuffer collects log output from concurrent handlers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	v     *fakeValidator
	cr    *fakeCreds
	wk    *fakeWaker
	st    store.Store
	clock *fakeClock
	logs  *syncBuffer
	// bearer is the token req sends (default goodToken)
	bearer string
}

// newFixture builds a server over a fresh sqlite store and fakes for oCIS.
// mod may change the Deps before NewServer. The test routes (testRoutes)
// are registered on every fixture.
func newFixture(t *testing.T, mod ...func(*Deps)) *fixture {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{
		t:     t,
		v:     &fakeValidator{tokens: map[string]auth.Identity{goodToken: aliceID, otherToken: bobID}},
		cr:    &fakeCreds{},
		wk:    &fakeWaker{},
		st:    st,
		clock: &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)},
		logs:  &syncBuffer{},
	}
	d := Deps{
		Store:     st,
		Validator: f.v,
		Auth:      f.cr,
		Gate:      fakeDecider{},
		Queue:     f.wk,
		Cfg: Settings{
			AllowedOrigin:   testOrigin,
			UserDailyTokens: 200000,
			RatePerSecond:   1000,
			Burst:           5000,
		},
		Log: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: f.clock.Now,
	}
	for _, m := range mod {
		m(&d)
	}
	srv, err := NewServer(d)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	f.srv = srv
	srv.actions.every = 0  // tests that need the cooldown turn it on
	srv.messages.every = 0 // likewise
	testRoutes(srv)
	f.h = srv.Handler()
	return f
}

type echoBody struct {
	Value string `json:"value"`
}

// testRoutes registers routes that exist only in tests, to exercise the
// pipeline before the real handlers exist.
func testRoutes(s *Server) {
	whoami := func(w http.ResponseWriter, r *http.Request) error {
		p, ok := principal(r.Context())
		if !ok {
			return errors.New("no principal")
		}
		writeJSON(w, http.StatusOK, p)
		return nil
	}
	s.handle("GET /agent-api/v1/test/whoami", whoami)
	s.handle("DELETE /agent-api/v1/test/whoami", whoami)
	s.handle("POST /agent-api/v1/test/echo", func(w http.ResponseWriter, r *http.Request) error {
		var b echoBody
		if err := decodeBody(w, r, &b); err != nil {
			return err
		}
		p, _ := principal(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"value": b.Value, "user": p.UserID})
		return nil
	})
	s.handle("POST /agent-api/v1/test/nobody", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	})
	s.handle("GET /agent-api/v1/test/panic", func(http.ResponseWriter, *http.Request) error {
		panic("SECRET-panic-value")
	})
	s.handle("GET /agent-api/v1/test/store", func(w http.ResponseWriter, r *http.Request) error {
		return s.d.Store.View(r.Context(), func(store.Tx) error { return nil })
	})
	s.handle("GET /agent-api/v1/test/deadline", func(w http.ResponseWriter, r *http.Request) error {
		dl, ok := r.Context().Deadline()
		writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "in": time.Until(dl).Seconds()})
		return nil
	})
	s.handle("GET /agent-api/v1/test/slow", func(w http.ResponseWriter, r *http.Request) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case <-time.After(3 * time.Second):
			writeJSON(w, http.StatusOK, map[string]bool{"deadline": false})
			return nil
		}
	}, withTimeout(50*time.Millisecond))
	s.handle("GET /agent-api/v1/test/controller", func(w http.ResponseWriter, r *http.Request) error {
		rc := http.NewResponseController(w)
		res := map[string]bool{
			"write_deadline": rc.SetWriteDeadline(time.Now().Add(time.Minute)) == nil,
			"read_deadline":  rc.SetReadDeadline(time.Now().Add(time.Minute)) == nil,
		}
		writeJSON(w, http.StatusOK, res)
		return rc.Flush()
	})
}

// failingStore makes View fail with an error that carries a secret.
type failingStore struct {
	store.Store
	err error
}

func (s failingStore) View(context.Context, func(store.Tx) error) error { return s.err }

// req builds a request with the defaults of a well-behaved client: the
// bearer of alice, the allowed Origin on state-changing methods and the JSON
// content type when there is a body.
func (f *fixture) req(method, target, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	tok := f.bearer
	if tok == "" {
		tok = goodToken
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func (f *fixture) serve(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

func (f *fixture) do(method, target, body string) *httptest.ResponseRecorder {
	return f.serve(f.req(method, target, body))
}

type errBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

// wantError checks status, the JSON content type and the error code.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) errBody {
	t.Helper()
	var b errBody
	if rec.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Errorf("body %q is not a JSON error: %v", rec.Body.String(), err)
		return b
	}
	if b.Error.Code != code {
		t.Errorf("code = %q, want %q", b.Error.Code, code)
	}
	if want := errorMessages[code]; want == "" || b.Error.Message != want {
		t.Errorf("message = %q, want the fixed text %q", b.Error.Message, want)
	}
	return b
}
