package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestNewServerValidatesDependencies(t *testing.T) {
	base := func() Deps {
		return Deps{
			Store: failingStore{}, Validator: &fakeValidator{}, Auth: &fakeCreds{}, Gate: fakeDecider{}, Queue: &fakeWaker{},
			Cfg: Settings{AllowedOrigin: testOrigin, UserDailyTokens: 1000, RatePerSecond: 1, Burst: 1},
		}
	}
	if _, err := NewServer(base()); err != nil {
		t.Fatalf("valid deps: %v", err)
	}
	for name, mod := range map[string]func(*Deps){
		"store":         func(d *Deps) { d.Store = nil },
		"validator":     func(d *Deps) { d.Validator = nil },
		"auth":          func(d *Deps) { d.Auth = nil },
		"gate":          func(d *Deps) { d.Gate = nil },
		"queue":         func(d *Deps) { d.Queue = nil },
		"no origin":     func(d *Deps) { d.Cfg.AllowedOrigin = "" },
		"origin path":   func(d *Deps) { d.Cfg.AllowedOrigin = "https://cloud.example.com/x" },
		"origin slash":  func(d *Deps) { d.Cfg.AllowedOrigin = "https://cloud.example.com/" },
		"origin scheme": func(d *Deps) { d.Cfg.AllowedOrigin = "ftp://cloud.example.com" },
		"rate":          func(d *Deps) { d.Cfg.RatePerSecond = 0 },
		"burst":         func(d *Deps) { d.Cfg.Burst = 0 },
		"daily tokens":  func(d *Deps) { d.Cfg.UserDailyTokens = 0 },
	} {
		d := base()
		mod(&d)
		if _, err := NewServer(d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestHealthzOnServer(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/agent-api/healthz", nil)
	r.Header.Set("Authorization", "garbage") // ignored: healthz skips auth
	rec := f.serve(r)
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}`+"\n" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if f.v.count() != 0 || len(f.cr.seenCalls()) != 0 {
		t.Errorf("healthz validated (%d) or called Seen (%d)", f.v.count(), len(f.cr.seenCalls()))
	}
	rec = f.serve(httptest.NewRequest(http.MethodPost, "/agent-api/healthz", nil))
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if a := rec.Header().Get("Allow"); !strings.Contains(a, "GET") {
		t.Errorf("Allow = %q", a)
	}
}

func TestUnmatchedPathsAreJSON404AndAgentUIIsNeverRouted(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		"/agent/", "/agent/anything", "/agent/chat", "/agent/healthz", "/agent/agent-api/healthz", "/agent",
		"/healthz", "/agent-api", "/agent-api/", "/agent-api/v1/nope", "/agent-api/v1/", "/agent-api/v1/test/whoami/",
		"/agent-api//healthz", "/agent-api/v1/../healthz", "/agent-api/./healthz", "/agent-api/v1/test/../../../agent/x",
		"/", "/agent-api/healthz/",
	} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			r := f.req(m, "/", "")
			r.URL.Path, r.URL.RawPath = p, ""
			r.RequestURI = p
			rec := f.serve(r)
			wantError(t, rec, http.StatusNotFound, "not_found")
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("%s %s redirects to %q", m, p, loc)
			}
		}
	}
	if f.v.count() != 0 {
		t.Errorf("unmatched paths reached the validator %d times", f.v.count())
	}
}

func TestWrongMethodIsJSON405WithAllow(t *testing.T) {
	f := newFixture(t)
	rec := f.do(http.MethodPut, "/agent-api/v1/test/whoami", "")
	wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	allow := rec.Header().Get("Allow")
	for _, m := range []string{"GET", "DELETE"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow = %q, missing %s", allow, m)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	f := newFixture(t)
	noAuth := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
	noAuth.Header.Del("Authorization")
	noOrigin := f.req(http.MethodPost, "/agent-api/v1/test/nobody", "")
	noOrigin.Header.Del("Origin")
	for name, r := range map[string]*http.Request{
		"healthz": httptest.NewRequest(http.MethodGet, "/agent-api/healthz", nil),
		"404":     httptest.NewRequest(http.MethodGet, "/agent/x", nil),
		"405":     httptest.NewRequest(http.MethodPost, "/agent-api/healthz", nil),
		"401":     noAuth,
		"403":     noOrigin,
		"200":     f.req(http.MethodGet, "/agent-api/v1/test/whoami", ""),
		"panic":   f.req(http.MethodGet, "/agent-api/v1/test/panic", ""),
	} {
		rec := f.serve(r)
		h := rec.Header()
		for k, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Cache-Control":                "no-store",
			"Referrer-Policy":              "no-referrer",
			"X-Frame-Options":              "DENY",
			"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", name, k, got, want)
			}
		}
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(h.Get("X-Request-Id")) {
			t.Errorf("%s: X-Request-Id = %q", name, h.Get("X-Request-Id"))
		}
	}
}

func TestRequestIDIsFreshAndInErrorsAndLogs(t *testing.T) {
	f := newFixture(t)
	r := f.req(http.MethodGet, "/agent-api/v1/nope", "")
	r.Header.Set("X-Request-Id", "client-chosen")
	rec := f.serve(r)
	b := wantError(t, rec, http.StatusNotFound, "not_found")
	id := rec.Header().Get("X-Request-Id")
	if id == "client-chosen" || b.Error.RequestID != id {
		t.Errorf("header %q, body %q", id, b.Error.RequestID)
	}
	if !strings.Contains(f.logs.String(), id) {
		t.Errorf("logs miss the request id %s: %s", id, f.logs.String())
	}
	if id2 := f.do(http.MethodGet, "/agent-api/v1/nope", "").Header().Get("X-Request-Id"); id2 == id {
		t.Error("request id reused")
	}
}

func TestOriginChecksForStateChangingMethods(t *testing.T) {
	type c struct {
		name   string
		method string
		mod    func(*http.Request)
		status int
	}
	cases := []c{
		{"post ok", http.MethodPost, func(*http.Request) {}, 200},
		{"post no sec-fetch-site", http.MethodPost, func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }, 200},
		{"post same-site", http.MethodPost, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"post origin upper case", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", "https://Cloud.Example.com") }, 403},
		{"post origin default port", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", testOrigin+":443") }, 403},
		{"post no origin", http.MethodPost, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"post wrong origin", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.com") }, 403},
		{"post origin trailing slash", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", testOrigin+"/") }, 403},
		{"post origin other scheme", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", "http://cloud.example.com") }, 403},
		{"post origin null", http.MethodPost, func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"post two origins", http.MethodPost, func(r *http.Request) { r.Header.Add("Origin", testOrigin) }, 403},
		{"post cross-site", http.MethodPost, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"post sec-fetch none", http.MethodPost, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") }, 403},
		{"post sec-fetch garbage", http.MethodPost, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "x") }, 403},
		{"delete no origin", http.MethodDelete, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"delete ok", http.MethodDelete, func(*http.Request) {}, 200},
		{"get no origin", http.MethodGet, func(r *http.Request) { r.Header.Del("Origin") }, 200},
		{"get cross origin", http.MethodGet, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.com") }, 200},
	}
	for _, tc := range cases {
		f := newFixture(t)
		path := "/agent-api/v1/test/nobody"
		if tc.method != http.MethodPost {
			path = "/agent-api/v1/test/whoami"
		}
		r := f.req(tc.method, path, "")
		tc.mod(r)
		rec := f.serve(r)
		if tc.status == 403 {
			wantError(t, rec, 403, "origin_rejected")
			// Origin is checked before auth: a cross-site request never reaches token validation
			if f.v.count() != 0 {
				t.Errorf("%s: validator called", tc.name)
			}
		} else if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.status, rec.Body.String())
		}
	}
}

func TestBearerParsing(t *testing.T) {
	long := strings.Repeat("a", 8193)
	cases := []struct {
		name     string
		headers  []string
		status   int
		validate bool // the validator must have been called
	}{
		{"missing", nil, 401, false},
		{"empty", []string{""}, 401, false},
		{"basic", []string{"Basic YWxpY2U6YXBwLXRva2Vu"}, 401, false},
		{"scheme only", []string{"Bearer"}, 401, false},
		{"scheme space", []string{"Bearer "}, 401, false},
		{"two tokens", []string{"Bearer a b"}, 401, false},
		{"double space", []string{"Bearer  " + goodToken}, 401, false},
		{"tab", []string{"Bearer\t" + goodToken}, 401, false},
		{"garbage chars", []string{"Bearer <script>"}, 401, false},
		{"non-ascii", []string{"Bearer tökén"}, 401, false},
		{"too long", []string{"Bearer " + long}, 401, false},
		{"two headers", []string{"Bearer " + goodToken, "Bearer " + goodToken}, 401, false},
		{"unknown token", []string{"Bearer unknown-token"}, 401, true},
		{"app token as bearer", []string{"Bearer AbCdEfApPtOkEn1234"}, 401, true},
		{"8 KiB token", []string{"Bearer " + long[:8192]}, 401, true},
		{"lower case scheme", []string{"bearer " + goodToken}, 200, true},
		{"upper case scheme", []string{"BEARER " + goodToken}, 200, true},
		{"jwt shaped", []string{"Bearer " + goodToken}, 200, true},
	}
	for _, tc := range cases {
		f := newFixture(t)
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Del("Authorization")
		for _, h := range tc.headers {
			r.Header.Add("Authorization", h)
		}
		rec := f.serve(r)
		if tc.status == 401 {
			wantError(t, rec, 401, "unauthenticated")
			if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("%s: WWW-Authenticate = %q", tc.name, got)
			}
			if len(f.cr.seenCalls()) != 0 {
				t.Errorf("%s: Seen called for an unauthenticated request", tc.name)
			}
		} else if rec.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.status)
		}
		if called := f.v.count() > 0; called != tc.validate {
			t.Errorf("%s: validator called = %v, want %v", tc.name, called, tc.validate)
		}
	}
}

func TestValidatorFailures(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{auth.ErrUnauthenticated, 401, "unauthenticated"},
		{fmt.Errorf("wrapped: %w", auth.ErrUnauthenticated), 401, "unauthenticated"},
		{auth.ErrNoLoginName, 422, "no_login_name"},
		{errors.New("ocisclient: GET /graph/v1.0/me: dial tcp: connection refused"), 502, "upstream_unavailable"},
		{fmt.Errorf("x: %w", store.ErrNotFound), 502, "upstream_unavailable"},
		{fmt.Errorf("ocisclient: GET /me: %w", context.DeadlineExceeded), 503, "unavailable"},
		{context.Canceled, 503, "unavailable"},
	} {
		f := newFixture(t)
		f.v.err = c.err
		rec := f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
		wantError(t, rec, c.status, c.code)
		if (c.status == 401) != (rec.Header().Get("WWW-Authenticate") == "Bearer") {
			t.Errorf("%v: WWW-Authenticate = %q", c.err, rec.Header().Get("WWW-Authenticate"))
		}
	}
}

func TestValidatorIdentityWithoutUserIDIsRefused(t *testing.T) {
	f := newFixture(t)
	f.v.tokens[goodToken] = auth.Identity{Username: "alice"}
	wantError(t, f.do(http.MethodGet, "/agent-api/v1/test/whoami", ""), 502, "upstream_unavailable")
}

func TestIdentityComesOnlyFromTheValidator(t *testing.T) {
	f := newFixture(t)
	r := f.req(http.MethodGet, "/agent-api/v1/test/whoami?user_id=u-bob", "")
	r.Header.Set("X-User", "u-bob")
	r.Header.Set("X-User-Id", "u-bob")
	r.Header.Set("X-Forwarded-User", "u-bob")
	rec := f.serve(r)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var p Principal
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	want := Principal{UserID: aliceID.UserID, Username: aliceID.Username, DisplayName: aliceID.DisplayName, DriveID: aliceID.DriveID, Subject: aliceID.UserID}
	if p != want {
		t.Errorf("principal = %+v, want %+v", p, want)
	}
	// a body field naming another user is an unknown field, not an identity
	rec = f.do(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"x","user_id":"u-bob"}`)
	wantError(t, rec, 400, "invalid_request")
	// the other user's token gives the other identity
	r = f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
	r.Header.Set("Authorization", "Bearer "+otherToken)
	_ = json.Unmarshal(f.serve(r).Body.Bytes(), &p)
	if p.UserID != bobID.UserID {
		t.Errorf("bob's token gave %+v", p)
	}
}

func TestSeenIsThrottledAndItsFailureDoesNotFail(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodGet, "/agent-api/v1/test/whoami", ""); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := f.cr.seenCalls(); len(got) != 1 || got[0] != aliceID.UserID {
		t.Fatalf("Seen calls = %v", got)
	}
	// within a minute: not again
	f.clock.Advance(59 * time.Second)
	f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	if n := len(f.cr.seenCalls()); n != 1 {
		t.Fatalf("Seen calls within the minute = %d", n)
	}
	// another user has their own window
	r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
	r.Header.Set("Authorization", "Bearer "+otherToken)
	f.serve(r)
	if got := f.cr.seenCalls(); len(got) != 2 || got[1] != bobID.UserID {
		t.Fatalf("Seen calls = %v", got)
	}
	// after the window: again, and a failure does not fail the request
	f.clock.Advance(2 * time.Second)
	f.cr.seenErr = errors.New("sqlite: SECRET-seen: disk I/O error")
	if rec := f.do(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"v"}`); rec.Code != 200 {
		t.Fatalf("status %d after a failing Seen", rec.Code)
	}
	if n := len(f.cr.seenCalls()); n != 3 {
		t.Errorf("Seen calls = %d, want 3", n)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, `"category":"seen"`) {
		t.Errorf("the Seen failure is not logged by category: %s", logs)
	}
	if strings.Contains(logs, "SECRET") {
		t.Errorf("logs carry the error text: %s", logs)
	}
	// a failure does not advance the throttle: the next request tries again
	f.cr.seenErr = nil
	f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	if n := len(f.cr.seenCalls()); n != 4 {
		t.Errorf("Seen calls after a failure = %d, want 4", n)
	}
	f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	if n := len(f.cr.seenCalls()); n != 4 {
		t.Errorf("Seen calls after a success = %d, want 4", n)
	}
	// a user who has not enrolled yet is not an error worth logging
	f.clock.Advance(61 * time.Second)
	f.cr.seenErr = fmt.Errorf("x: %w", store.ErrNotFound)
	before := strings.Count(f.logs.String(), `"category":"seen"`)
	f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	if strings.Count(f.logs.String(), `"category":"seen"`) != before {
		t.Error("a not-enrolled Seen is logged as a failure")
	}
}

func TestSeenThrottleIsBounded(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	st := newSeenThrottle(time.Minute, 3, c.Now)
	for i := range 10 {
		u := fmt.Sprintf("u%d", i)
		if !st.due(u) {
			t.Fatalf("%s not due", u)
		}
		st.done(u)
		if n := st.size(); n > 3 {
			t.Fatalf("size = %d", n)
		}
	}
	// a user that could not be recorded (the map was full of fresh entries)
	// is simply due again: Seen is called more often, never less
	if !st.due("u9") {
		t.Error("an unrecorded user is not due")
	}
	c.Advance(time.Minute + time.Second)
	st.done("u10") // stale entries are swept to make room
	if st.due("u10") {
		t.Error("u10 not recorded after the sweep")
	}
}

func TestStrictJSONBodies(t *testing.T) {
	big := `{"value":"` + strings.Repeat("x", 2<<20) + `"}`
	deep := strings.Repeat(`{"a":`, 40) + "1" + strings.Repeat("}", 40)
	cases := []struct {
		name, ct, body string
		status         int
		code           string
	}{
		{"ok", "application/json", `{"value":"v"}`, 200, ""},
		{"ok charset", "application/json; charset=utf-8", `{"value":"v"}`, 200, ""},
		{"ok charset upper", "Application/JSON; charset=UTF-8", `{"value":"v"}`, 200, ""},
		{"ok whitespace", "application/json", " \n{\"value\":\"v\"}\n ", 200, ""},
		{"unknown field", "application/json", `{"value":"v","extra":1}`, 400, "invalid_request"},
		{"two values", "application/json", `{"value":"v"}{"value":"w"}`, 400, "invalid_request"},
		{"trailing garbage", "application/json", `{"value":"v"}x`, 400, "invalid_request"},
		{"duplicate key", "application/json", `{"value":"v","value":"w"}`, 400, "invalid_request"},
		{"case variant key", "application/json", `{"value":"v","Value":"w"}`, 400, "invalid_request"},
		{"null", "application/json", `null`, 400, "invalid_request"},
		{"array", "application/json", `[{"value":"v"}]`, 400, "invalid_request"},
		{"wrong type", "application/json", `{"value":1}`, 400, "invalid_request"},
		{"malformed", "application/json", `{"value":`, 400, "invalid_request"},
		{"too deep", "application/json", deep, 400, "invalid_request"},
		{"text/plain", "text/plain", `{"value":"v"}`, 415, "unsupported_media_type"},
		{"form", "application/x-www-form-urlencoded", `value=v`, 415, "unsupported_media_type"},
		{"no content type", "", `{"value":"v"}`, 415, "unsupported_media_type"},
		{"bad charset", "application/json; charset=latin1", `{"value":"v"}`, 415, "unsupported_media_type"},
		{"bad params", "application/json; boundary=x", `{"value":"v"}`, 415, "unsupported_media_type"},
		{"json suffix", "application/vnd.evil+json", `{"value":"v"}`, 415, "unsupported_media_type"},
		{"2 MiB", "application/json", big, 413, "too_large"},
	}
	f := newFixture(t)
	for _, tc := range cases {
		r := f.req(http.MethodPost, "/agent-api/v1/test/echo", tc.body)
		if tc.ct == "" {
			r.Header.Del("Content-Type")
		} else {
			r.Header.Set("Content-Type", tc.ct)
		}
		rec := f.serve(r)
		if tc.status == 200 {
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":"v"`) {
				t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			continue
		}
		t.Run(tc.name, func(t *testing.T) { wantError(t, rec, tc.status, tc.code) })
	}
}

func TestBodyRules(t *testing.T) {
	f := newFixture(t)
	// a large body without a Content-Length (chunked) is cut at the limit
	r := f.req(http.MethodPost, "/agent-api/v1/test/echo", "")
	r.Body = io.NopCloser(strings.NewReader(`{"value":"` + strings.Repeat("x", 2<<20) + `"}`))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/json")
	wantError(t, f.serve(r), 413, "too_large")

	// an empty POST needs no content type, but decodeBody requires an object
	wantError(t, f.do(http.MethodPost, "/agent-api/v1/test/echo", ""), 400, "invalid_request")
	if rec := f.do(http.MethodPost, "/agent-api/v1/test/nobody", ""); rec.Code != 200 {
		t.Errorf("empty POST = %d", rec.Code)
	}
	// GET and DELETE carry no body
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		r := f.req(m, "/agent-api/v1/test/whoami", `{}`)
		wantError(t, f.serve(r), 400, "invalid_request")
	}
}

func TestPanicIsJSON500AndLogged(t *testing.T) {
	f := newFixture(t)
	rec := f.do(http.MethodGet, "/agent-api/v1/test/panic", "")
	wantError(t, rec, 500, "internal")
	if !strings.Contains(f.logs.String(), `"category":"panic"`) {
		t.Errorf("no panic log line: %s", f.logs.String())
	}
}

// Errors carrying secrets (a store error, a validator error, a panic value,
// a failing Seen) never reach the response or the logs; neither do the
// bearer, the query string or the body.
func TestSecretsNeverLeak(t *testing.T) {
	const secret = "SECRET"
	f := newFixture(t, func(d *Deps) {
		d.Store = failingStore{Store: d.Store, err: errors.New("sqlite: SECRET-store: constraint failed")}
	})
	var out strings.Builder
	record := func(rec *httptest.ResponseRecorder) {
		out.WriteString(rec.Body.String())
		for k, v := range rec.Header() {
			out.WriteString(k + ": " + strings.Join(v, ",") + "\n")
		}
	}
	f.cr.seenErr = errors.New("sqlite: SECRET-seen")
	rec := f.do(http.MethodGet, "/agent-api/v1/test/store", "")
	wantError(t, rec, 500, "internal")
	record(rec)
	record(f.do(http.MethodGet, "/agent-api/v1/test/panic", ""))
	record(f.do(http.MethodGet, "/agent-api/v1/test/whoami?access_token=SECRET-query", ""))
	record(f.do(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"SECRET-body","x":1}`))
	r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
	r.Header.Set("Authorization", "Bearer SECRET-bearer")
	record(f.serve(r))
	f.v.expireCache() // like the real validator, a cached token would skip oCIS
	f.v.err = errors.New("ocisclient: GET /graph/v1.0/me: SECRET-validator")
	rec = f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	wantError(t, rec, 502, "upstream_unavailable")
	record(rec)

	if strings.Contains(out.String(), secret) {
		t.Errorf("a response carries a secret:\n%s", out.String())
	}
	logs := f.logs.String()
	if strings.Contains(logs, secret) || strings.Contains(logs, goodToken) {
		t.Errorf("logs carry a secret:\n%s", logs)
	}
	for _, cat := range []string{`"category":"internal"`, `"category":"panic"`, `"category":"upstream_unavailable"`, `"category":"seen"`} {
		if !strings.Contains(logs, cat) {
			t.Errorf("logs miss %s:\n%s", cat, logs)
		}
	}
}

func TestRequestLogLine(t *testing.T) {
	f := newFixture(t)
	f.do(http.MethodGet, "/agent-api/v1/test/whoami?q=1", "")
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if strings.Contains(l, `"msg":"api: request"`) {
			_ = json.Unmarshal([]byte(l), &line)
		}
	}
	if line == nil {
		t.Fatalf("no request line: %s", f.logs.String())
	}
	if line["route"] != "GET /agent-api/v1/test/whoami" || line["status"] != float64(200) || line["user"] != aliceID.UserID || line["method"] != "GET" {
		t.Errorf("line = %v", line)
	}
	if strings.Contains(f.logs.String(), "q=1") {
		t.Error("the query string is logged")
	}
}

// The pre-auth IP bucket counts failed authentications only. When it is
// empty, a request with a token the validator has cached still proceeds (a
// flood from the shared proxy IP does not lock out real users), anything
// else is refused before any validator call.
func TestAuthFailureLimitByIP(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	failBurst := ipBurstFactor * 1
	send := func(ip, authz string) *httptest.ResponseRecorder {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", authz)
		r.Header.Set("X-Forwarded-For", "10.9.9.9") // never trusted
		r.RemoteAddr = ip
		return f.serve(r)
	}
	// alice validated once: her token is in the validator's cache
	if rec := send("10.0.0.1:1", "Bearer "+goodToken); rec.Code != 200 {
		t.Fatalf("alice: %d", rec.Code)
	}
	// successful requests (even user-limited ones) never drain the bucket
	for range 3 * failBurst {
		send("10.0.0.1:1", "Bearer "+goodToken)
	}
	// the attacker: garbage and validator-rejected tokens alike
	for i := range failBurst {
		authz := "Basic Z2FyYmFnZQ=="
		if i%2 == 0 {
			authz = "Bearer wrong"
		}
		if rec := send("10.0.0.1:2", authz); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	calls := f.v.count()
	rec := send("10.0.0.1:3", "Bearer wrong")
	wantError(t, rec, 429, "rate_limited")
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q", ra)
	}
	wantError(t, send("10.0.0.1:3", "Basic Z2FyYmFnZQ=="), 429, "rate_limited")
	// bob's token is valid but not cached: refused without a validator call
	wantError(t, send("10.0.0.1:4", "Bearer "+otherToken), 429, "rate_limited")
	if f.v.count() != calls {
		t.Error("a blocked request reached the validator")
	}
	// alice (cached) still gets through from the same IP, without a validator call
	f.clock.Advance(time.Second) // her own per-user bucket refills; the IP bucket gets 10 tokens back...
	for range failBurst {
		send("10.0.0.1:2", "Bearer wrong") // ...which the attacker drains again
	}
	calls = f.v.count()
	if rec := send("10.0.0.1:5", "Bearer "+goodToken); rec.Code != 200 {
		t.Errorf("cached user during the flood: %d", rec.Code)
	}
	if f.v.count() != calls {
		t.Error("a cached token was sent to the validator")
	}
	wantError(t, send("10.0.0.1:5", "Bearer "+otherToken), 429, "rate_limited")
	if f.v.count() != calls {
		t.Error("a blocked request reached the validator")
	}
	// the gate comes before the Origin check
	r := f.req(http.MethodPost, "/agent-api/v1/test/nobody", "")
	r.Header.Set("Authorization", "Bearer wrong")
	r.Header.Del("Origin")
	r.RemoteAddr = "10.0.0.1:1"
	wantError(t, f.serve(r), 429, "rate_limited")
	// IPv4-mapped IPv6 is the same client
	wantError(t, send("[::ffff:10.0.0.1]:1", "Bearer wrong"), 429, "rate_limited")
	// another IP has its own bucket
	if rec := send("10.0.0.2:1", "Bearer wrong"); rec.Code != 401 {
		t.Errorf("other IP: %d", rec.Code)
	}
	// healthz is never limited
	for range 3 * failBurst {
		r := httptest.NewRequest(http.MethodGet, "/agent-api/healthz", nil)
		r.RemoteAddr = "10.0.0.1:1"
		if f.serve(r).Code != 200 {
			t.Fatal("healthz limited")
		}
	}
	f.clock.Advance(time.Second / ipRateFactor)
	if rec := send("10.0.0.1:1", "Bearer wrong"); rec.Code != 401 {
		t.Errorf("after refill: %d", rec.Code)
	}
}

func TestAuthFailureLimitKeysIPv6By64(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	send := func(ip string) int {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer wrong")
		r.RemoteAddr = ip
		return f.serve(r).Code
	}
	for i := range ipBurstFactor {
		if c := send(fmt.Sprintf("[2001:db8:0:0::%x]:1", i+1)); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := send("[2001:db8::ffff:1]:1"); c != 429 {
		t.Errorf("same /64: %d", c)
	}
	if c := send("[2001:db8::1%eth0]:1"); c != 429 {
		t.Errorf("same /64 with a zone: %d", c)
	}
	if c := send("[2001:db8:0:1::1]:1"); c != 401 {
		t.Errorf("other /64: %d", c)
	}
}

func TestClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.1:1234":          "10.0.0.1",
		"[::ffff:10.0.0.1]:1":    "10.0.0.1",
		"[2001:db8::1]:1":        "2001:db8::/64",
		"[2001:db8::abcd:1]:443": "2001:db8::/64",
		"[fe80::1%eth0]:1":       "fe80::/64",
		"garbage":                "garbage",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = in
		if got := clientKey(r); got != want {
			t.Errorf("clientKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidatorOutageDoesNotChargeTheIP(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	f.v.err = errors.New("ocisclient: GET /me: connection refused")
	for i := range 3 * ipBurstFactor {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer some-token")
		if c := f.serve(r).Code; c != 502 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
}

func TestPostAuthRateLimitByUser(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 2, 2 })
	get := func(tok string) *httptest.ResponseRecorder {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer "+tok)
		return f.serve(r)
	}
	for range 2 {
		if rec := get(goodToken); rec.Code != 200 {
			t.Fatalf("alice: %d", rec.Code)
		}
	}
	rec := get(goodToken)
	wantError(t, rec, 429, "rate_limited")
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	// the user limit comes before the body checks
	r := f.req(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"v"}`)
	r.Header.Set("Content-Type", "text/plain")
	wantError(t, f.serve(r), 429, "rate_limited")
	// same IP, other user: own bucket
	if rec := get(otherToken); rec.Code != 200 {
		t.Errorf("bob: %d", rec.Code)
	}
	f.clock.Advance(500 * time.Millisecond)
	if rec := get(goodToken); rec.Code != 200 {
		t.Errorf("alice after refill: %d", rec.Code)
	}
	wantError(t, get(goodToken), 429, "rate_limited")
}

func TestPipelineOrderAuthBeforeBody(t *testing.T) {
	f := newFixture(t)
	r := f.req(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"v"}`)
	r.Header.Set("Authorization", "Bearer wrong")
	r.Header.Set("Content-Type", "text/plain")
	wantError(t, f.serve(r), 401, "unauthenticated")
}

func TestHandlerDeadlines(t *testing.T) {
	f := newFixture(t)
	rec := f.do(http.MethodGet, "/agent-api/v1/test/deadline", "")
	var got struct {
		OK bool    `json:"ok"`
		In float64 `json:"in"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !got.OK || got.In > 30 || got.In < 25 {
		t.Errorf("deadline = %+v, want about 30s", got)
	}
	start := time.Now()
	wantError(t, f.do(http.MethodGet, "/agent-api/v1/test/slow", ""), 503, "unavailable")
	if time.Since(start) > 5*time.Second {
		t.Error("the route timeout did not apply")
	}
}

// Over a real server the ResponseController reaches the connection through
// the pipeline's response writer wrappers (they implement Unwrap).
func TestResponseControllerOverARealServer(t *testing.T) {
	f := newFixture(t)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agent-api/v1/test/controller", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != `{"read_deadline":true,"write_deadline":true}`+"\n" {
		t.Errorf("%d %s", res.StatusCode, body)
	}
	// a second request on the same connection still works
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/agent-api/healthz", nil)
	res2, err := ts.Client().Do(req)
	if err != nil || res2.StatusCode != 200 {
		t.Fatalf("second request: %v", err)
	}
	_ = res2.Body.Close()
}

func TestSeenRunsBeforeTheUserLimit(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 0.001, 1 })
	if rec := f.do(http.MethodGet, "/agent-api/v1/test/whoami", ""); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	f.clock.Advance(61 * time.Second) // Seen is due again, the user bucket is still empty
	wantError(t, f.do(http.MethodGet, "/agent-api/v1/test/whoami", ""), 429, "rate_limited")
	if n := len(f.cr.seenCalls()); n != 2 {
		t.Errorf("Seen calls = %d, want 2 (the limited request is a visit too)", n)
	}
}

// A panic before anything was written is a 500; after the response started
// the connection is aborted, so the client never sees a clean, truncated 200.
func TestPanicAfterTheResponseStartedAbortsTheConnection(t *testing.T) {
	f := newFixture(t)
	f.srv.handle("GET /agent-api/v1/test/partial", func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partial":`))
		_ = http.NewResponseController(w).Flush()
		panic("SECRET-late-panic")
	})
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET /agent-api/v1/test/partial HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + goodToken + "\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if err == nil {
		t.Fatalf("the truncated response ended cleanly: %d %q", res.StatusCode, body)
	}
	if !strings.Contains(f.logs.String(), `"category":"panic"`) || strings.Contains(f.logs.String(), "SECRET") {
		t.Errorf("logs: %s", f.logs.String())
	}

	// in-process: the pipeline re-panics with http.ErrAbortHandler
	for _, path := range []string{"/agent-api/v1/test/partial", "/agent-api/v1/test/abort"} {
		if path == "/agent-api/v1/test/abort" {
			f.srv.handle("GET "+path, func(http.ResponseWriter, *http.Request) error { panic(http.ErrAbortHandler) })
		}
		func() {
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Errorf("%s: recovered %v, want http.ErrAbortHandler", path, v)
				}
			}()
			f.serve(f.req(http.MethodGet, path, ""))
		}()
	}
	// an abort before anything was written is logged without a made-up 200
	found := false
	for _, l := range strings.Split(f.logs.String(), "\n") {
		if strings.Contains(l, `"route":"GET /agent-api/v1/test/abort"`) && strings.Contains(l, `"msg":"api: request"`) {
			found = strings.Contains(l, `"status":0`) && strings.Contains(l, `"category":"aborted"`)
		}
	}
	if !found {
		t.Errorf("no aborted request line with status 0: %s", f.logs.String())
	}
}

func TestHandlersCanFlushThroughTheWrapper(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	// without a working flush the client would wait for the handler, which
	// waits for the client: a watchdog breaks that tie
	watchdog := time.AfterFunc(5*time.Second, free)
	defer watchdog.Stop()
	f.srv.handle("GET /agent-api/v1/test/stream", func(w http.ResponseWriter, r *http.Request) error {
		fl, ok := w.(http.Flusher)
		if !ok {
			return errors.New("no flusher")
		}
		_, _ = w.Write([]byte("first\n"))
		fl.Flush()
		<-release // the handler is still running while the client reads
		_, _ = w.Write([]byte("second\n"))
		return nil
	})
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agent-api/v1/test/stream", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	res, err := ts.Client().Do(req)
	if err != nil {
		free()
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	br := bufio.NewReader(res.Body)
	line, err := br.ReadString('\n')
	select {
	case <-release:
		t.Fatal("the flushed bytes did not arrive while the handler was running")
	default:
	}
	free()
	if err != nil || line != "first\n" {
		t.Fatalf("first line: %q %v", line, err)
	}
	if rest, _ := io.ReadAll(br); string(rest) != "second\n" {
		t.Errorf("rest = %q", rest)
	}
}

func TestWriteDeadlineFiresOverARealServer(t *testing.T) {
	old := writeGrace
	writeGrace = 0
	t.Cleanup(func() { writeGrace = old })
	f := newFixture(t)
	f.srv.handle("GET /agent-api/v1/test/late", func(w http.ResponseWriter, r *http.Request) error {
		time.Sleep(300 * time.Millisecond) // ignores its context on purpose
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
		return nil
	}, withTimeout(100*time.Millisecond))
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agent-api/v1/test/late", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	res, err := ts.Client().Do(req)
	if err == nil {
		body, rerr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if rerr == nil {
			t.Fatalf("a write after the deadline arrived: %d, %d bytes", res.StatusCode, len(body))
		}
	}
}

type caseDst struct {
	DailyTokens int                `json:"daily_tokens"`
	AutoApprove bool               `json:"auto_approve_write"`
	Nested      *caseInner         `json:"nested"`
	List        []caseInner        `json:"list"`
	Models      map[string]*string `json:"models"`
	Raw         json.RawMessage    `json:"raw"`
	Untagged    string
	skipped     string                 // unexported: not a JSON field
	Ignored     string                 `json:"-"`
	Free        map[string]any         `json:"free"`
	Deep        map[string]caseInner   `json:"deep"`
	DeepList    map[string][]caseInner `json:"deep_list"`
	Custom      customUnmarshal        `json:"custom"`
}

// customUnmarshal decodes itself: its keys are its own business.
type customUnmarshal struct {
	Name string `json:"name"`
}

func (c *customUnmarshal) UnmarshalJSON([]byte) error { return nil }

type caseInner struct {
	Name string `json:"name"`
}

func TestDecodeBodyRequiresExactKeyCase(t *testing.T) {
	ok := []string{
		`{"daily_tokens":1}`,
		`{"auto_approve_write":true,"nested":{"name":"a"},"list":[{"name":"b"}]}`,
		`{"models":{"CHAT":"x"}}`, // map keys are the handler's to validate
		`{"raw":{"ANY":1}}`,
		`{"Untagged":"x"}`,
		`{"free":{"Name":1}}`,
		`{"nested":null,"list":null}`,
		`{"deep":{"ANYKEY":{"name":"a"}}}`,
		`{"deep_list":{"K":[{"name":"a"}]}}`,
		`{"custom":{"NAME":1}}`,
	}
	bad := []string{
		`{"Daily_Tokens":1}`,
		`{"AUTO_APPROVE_WRITE":true}`,
		`{"nested":{"Name":"a"}}`,
		`{"list":[{"name":"a"},{"NAME":"b"}]}`,
		`{"untagged":"x"}`,
		`{"Ignored":"x"}`,
		`{"skipped":"x"}`,
		`{"deep":{"x":{"NAME":"a"}}}`,
		`{"deep_list":{"k":[{"name":"a"},{"Name":"b"}]}}`,
	}
	run := func(body string) error {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var dst caseDst
		return decodeBody(httptest.NewRecorder(), r, &dst)
	}
	for _, b := range ok {
		if err := run(b); err != nil {
			t.Errorf("%s: %v", b, err)
		}
	}
	for _, b := range bad {
		if err := run(b); !errors.Is(err, errBadJSON) {
			t.Errorf("%s: err = %v, want errBadJSON", b, err)
		}
	}
	// over HTTP it is a 400
	f := newFixture(t)
	wantError(t, f.do(http.MethodPost, "/agent-api/v1/test/echo", `{"Value":"v"}`), 400, "invalid_request")
}

func TestAuthFailureReservationBoundsConcurrentValidation(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	f.v.delay = 50 * time.Millisecond
	const n = 200
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
			r.Header.Set("Authorization", "Bearer wrong")
			r.RemoteAddr = "10.0.0.1:1"
			codes <- f.serve(r).Code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if calls := f.v.count(); calls > ipBurstFactor {
		t.Errorf("%d validator calls for %d concurrent bad tokens, want at most the bucket (%d)", calls, n, ipBurstFactor)
	}
	if count[401] != ipBurstFactor || count[429] != n-ipBurstFactor {
		t.Errorf("codes = %v", count)
	}
}

func TestDisconnectedClientsStayCharged(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client sent its garbage and hung up
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "").WithContext(ctx)
		r.Header.Set("Authorization", "Bearer wrong")
		r.RemoteAddr = "10.0.0.1:1"
		f.serve(r)
	}
	if calls := f.v.count(); calls > ipBurstFactor {
		t.Errorf("%d validator calls from hung-up clients, want at most %d", calls, ipBurstFactor)
	}
	r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
	r.Header.Set("Authorization", "Bearer wrong")
	r.RemoteAddr = "10.0.0.1:1"
	wantError(t, f.serve(r), 429, "rate_limited")
}

func TestReservationsAreRefundedWhenNotTheClientsFault(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	send := func(mod func(*http.Request)) int {
		r := f.req(http.MethodPost, "/agent-api/v1/test/nobody", "")
		r.Header.Set("Authorization", "Bearer some-token")
		r.RemoteAddr = "10.0.0.1:1"
		mod(r)
		return f.serve(r).Code
	}
	for range 3 * ipBurstFactor { // a cross-site request is refused before validation
		if c := send(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example.com") }); c != 403 {
			t.Fatalf("cross-site: %d", c)
		}
	}
	f.v.err = auth.ErrNoLoginName
	for range 3 * ipBurstFactor {
		if c := send(func(*http.Request) {}); c != 422 {
			t.Fatalf("no login name: %d", c)
		}
	}
	f.v.err = nil
	// the bucket is still full: ipBurstFactor rejections fit
	for i := range ipBurstFactor {
		if c := send(func(*http.Request) {}); c != 401 {
			t.Fatalf("rejection %d: %d", i, c)
		}
	}
	if c := send(func(*http.Request) {}); c != 429 {
		t.Errorf("after the bucket: %d", c)
	}
}

// A token oCIS accepted before (auth.Validator.Known) may still be validated
// while the IP is blocked, after its cache entry expired; an unknown one may
// not; a known token that is now rejected is charged and forgotten.
func TestKnownTokensWhileTheIPIsBlocked(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	send := func(tok string) int {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer "+tok)
		r.RemoteAddr = "10.0.0.1:1"
		return f.serve(r).Code
	}
	if c := send(goodToken); c != 200 {
		t.Fatalf("alice: %d", c)
	}
	for range ipBurstFactor {
		send("wrong")
	}
	f.v.expireCache()            // the 60s cache is gone, the flood goes on
	f.clock.Advance(time.Second) // alice's own bucket refills
	for range ipBurstFactor {
		send("wrong")
	}
	calls := f.v.count()
	if c := send(goodToken); c != 200 {
		t.Errorf("known token while blocked: %d", c)
	}
	if f.v.count() != calls+1 {
		t.Errorf("the known token was not validated (%d calls)", f.v.count()-calls)
	}
	calls = f.v.count()
	if c := send(otherToken); c != 429 || f.v.count() != calls {
		t.Errorf("unknown token while blocked: %d, %d calls", c, f.v.count()-calls)
	}
	// alice's token is revoked: rejected once, then unknown
	delete(f.v.tokens, goodToken)
	f.v.expireCache()
	f.clock.Advance(time.Second)
	for range ipBurstFactor {
		send("wrong")
	}
	if c := send(goodToken); c != 401 {
		t.Errorf("revoked known token: %d", c)
	}
	calls = f.v.count()
	if c := send(goodToken); c != 429 || f.v.count() != calls {
		t.Errorf("forgotten token: %d, %d calls", c, f.v.count()-calls)
	}
}

func TestARejectedKnownTokenIsCharged(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	send := func(tok string) int {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer "+tok)
		r.RemoteAddr = "10.0.0.1:1"
		return f.serve(r).Code
	}
	send(goodToken)
	delete(f.v.tokens, goodToken)
	f.v.expireCache()
	if c := send(goodToken); c != 401 {
		t.Fatalf("revoked: %d", c)
	}
	for i := range ipBurstFactor - 1 {
		if c := send("wrong"); c != 401 {
			t.Fatalf("rejection %d: %d", i, c)
		}
	}
	if c := send("wrong"); c != 429 {
		t.Errorf("the known token's rejection was not charged: %d", c)
	}
}

type flushErrWriter struct {
	httptest.ResponseRecorder
	err error
}

func (w *flushErrWriter) FlushError() error { return w.err }

func TestFlushErrorsAreReturned(t *testing.T) {
	broken := errors.New("broken pipe")
	sw := &statusWriter{ResponseWriter: &flushErrWriter{ResponseRecorder: *httptest.NewRecorder(), err: broken}}
	if err := http.NewResponseController(sw).Flush(); !errors.Is(err, broken) {
		t.Errorf("Flush = %v, want the writer's error", err)
	}
	if !sw.wrote || sw.status != 200 {
		t.Errorf("wrote=%v status=%d", sw.wrote, sw.status)
	}
	sw.Flush() // the http.Flusher form ignores the error
}
