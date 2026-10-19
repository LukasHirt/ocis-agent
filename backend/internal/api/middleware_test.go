package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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
		{"post same-site", http.MethodPost, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 200},
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

func TestSeenOncePerRequestAndItsFailureDoesNotFail(t *testing.T) {
	f := newFixture(t)
	if rec := f.do(http.MethodGet, "/agent-api/v1/test/whoami", ""); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := f.cr.seenCalls(); len(got) != 1 || got[0] != aliceID.UserID {
		t.Fatalf("Seen calls = %v", got)
	}
	f.cr.seenErr = errors.New("sqlite: SECRET-seen: disk I/O error")
	if rec := f.do(http.MethodPost, "/agent-api/v1/test/echo", `{"value":"v"}`); rec.Code != 200 {
		t.Fatalf("status %d after a failing Seen", rec.Code)
	}
	if len(f.cr.seenCalls()) != 2 {
		t.Errorf("Seen calls = %d", len(f.cr.seenCalls()))
	}
	logs := f.logs.String()
	if !strings.Contains(logs, `"category":"seen"`) {
		t.Errorf("the Seen failure is not logged by category: %s", logs)
	}
	if strings.Contains(logs, "SECRET") {
		t.Errorf("logs carry the error text: %s", logs)
	}
	// a user who has not enrolled yet is not an error worth logging
	f.cr.seenErr = fmt.Errorf("x: %w", store.ErrNotFound)
	before := strings.Count(f.logs.String(), `"category":"seen"`)
	f.do(http.MethodGet, "/agent-api/v1/test/whoami", "")
	if strings.Count(f.logs.String(), `"category":"seen"`) != before {
		t.Error("a not-enrolled Seen is logged as a failure")
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

func TestPreAuthRateLimitByIP(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.RatePerSecond, d.Cfg.Burst = 1, 1 })
	ipBurst := ipBurstFactor * 1
	bad := func(ip string) *httptest.ResponseRecorder {
		r := f.req(http.MethodGet, "/agent-api/v1/test/whoami", "")
		r.Header.Set("Authorization", "Bearer wrong")
		r.Header.Set("X-Forwarded-For", "10.9.9.9") // never trusted
		r.RemoteAddr = ip + ":1234"
		return f.serve(r)
	}
	for i := range ipBurst {
		if rec := bad("10.0.0.1"); rec.Code != 401 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	calls := f.v.count()
	rec := bad("10.0.0.1")
	wantError(t, rec, 429, "rate_limited")
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After = %q", ra)
	}
	if f.v.count() != calls {
		t.Error("a rate-limited request reached the validator")
	}
	// the IP limit comes before the Origin check too
	r := f.req(http.MethodPost, "/agent-api/v1/test/nobody", "")
	r.Header.Del("Origin")
	r.RemoteAddr = "10.0.0.1:1"
	wantError(t, f.serve(r), 429, "rate_limited")
	// another IP has its own bucket
	if rec := bad("10.0.0.2"); rec.Code != 401 {
		t.Errorf("other IP: %d", rec.Code)
	}
	// healthz is never limited
	for range 3 * ipBurst {
		r := httptest.NewRequest(http.MethodGet, "/agent-api/healthz", nil)
		r.RemoteAddr = "10.0.0.1:1"
		if f.serve(r).Code != 200 {
			t.Fatal("healthz limited")
		}
	}
	f.clock.Advance(time.Second / ipRateFactor)
	if rec := bad("10.0.0.1"); rec.Code != 401 {
		t.Errorf("after refill: %d", rec.Code)
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
	// Seen comes before the user limit: it ran for the limited request too
	if n := len(f.cr.seenCalls()); n != 3 {
		t.Errorf("Seen calls = %d, want 3", n)
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
