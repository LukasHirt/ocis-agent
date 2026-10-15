package ocisclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

const secretValue = "s3cr3t-token-value"

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestCredsApplyToRequests(t *testing.T) {
	var gotUser, gotPass, gotAuth string
	var gotBasic bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotBasic = r.BasicAuth()
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})

	if _, err := c.do(context.Background(), AppToken("einstein", secretValue), http.MethodGet, "/x", nil, nil, nil, maxBody); err != nil {
		t.Fatal(err)
	}
	if !gotBasic || gotUser != "einstein" || gotPass != secretValue {
		t.Fatalf("basic auth = %v %q %q", gotBasic, gotUser, gotPass)
	}

	if _, err := c.do(context.Background(), Bearer(secretValue), http.MethodGet, "/x", nil, nil, nil, maxBody); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+secretValue {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestMissingCredentialsAreRejectedBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	for _, cr := range []Creds{{}, AppToken("", secretValue), AppToken("u", ""), Bearer("")} {
		_, err := c.do(context.Background(), cr, http.MethodGet, "/x", nil, nil, nil, maxBody)
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("%v: err = %v, want ErrNoCredentials", cr, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a request was sent without credentials")
	}
}

func TestCredentialsNeverPrint(t *testing.T) {
	cr := AppToken("einstein", Secret(secretValue))
	s := Secret(secretValue)
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "creds", cr, "secret", s)
	type wrapper struct{ Token Secret }
	js, err := json.Marshal(wrapper{Token: s})
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		fmt.Sprintf("%v %+v %#v %s", cr, cr, cr, cr),
		fmt.Sprintf("%v %+v %#v %s", s, s, s, s),
		fmt.Sprintf("%v %+v", wrapper{Token: s}, &wrapper{Token: s}),
		buf.String(),
		string(js),
	}
	for i, out := range outputs {
		if strings.Contains(out, secretValue) {
			t.Fatalf("output %d leaks the secret: %s", i, out)
		}
	}
	if s.Reveal() != secretValue {
		t.Fatal("Reveal must return the raw value")
	}
	if cr.Kind() != "app" || Bearer(s).Kind() != "oidc" || (Creds{}).Kind() != "" {
		t.Fatal("unexpected Kind values")
	}
}

func TestNewRequiresAnHTTPOrigin(t *testing.T) {
	for _, bad := range []string{"", "localhost:9200", "ftp://h", "https://", "https://h/path", "https://h?x=1", "https://u:p@h/x", "://bad"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) should fail", bad)
		} else if strings.Contains(err.Error(), "p@h") {
			t.Errorf("error leaks the URL: %v", err)
		}
	}
	if _, err := New("https://ocis.example.org/"); err != nil {
		t.Fatalf("trailing slash must be accepted: %v", err)
	}
}

func TestStatusErrorsMatchSentinelsAndHideTheQuery(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{{401, ErrUnauthorized}, {403, ErrForbidden}, {404, ErrNotFound}}
	for _, tc := range cases {
		e := statusError("GET", "/p", tc.status, nil)
		if !errors.Is(e, tc.want) {
			t.Errorf("status %d should match %v", tc.status, tc.want)
		}
	}
	e := statusError("GET", "/p", 500, nil)
	for _, s := range []error{ErrUnauthorized, ErrForbidden, ErrNotFound, ErrPrecondition, ErrExists} {
		if errors.Is(e, s) {
			t.Errorf("500 must not match %v", s)
		}
	}
	if !errors.Is(statusError("PUT", "/p", 409, ErrPrecondition), ErrPrecondition) {
		t.Error("an explicit kind must win")
	}
	var se *StatusError
	if !errors.As(fmt.Errorf("wrapped: %w", e), &se) || se.Status != 500 || se.Method != "GET" || se.Path != "/p" {
		t.Fatalf("errors.As failed: %+v", se)
	}
	if got := e.Error(); got != "ocisclient: GET /p: status 500" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestTransportErrorsHideTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close() // connection refused
	_, err = c.do(context.Background(), Bearer(secretValue), http.MethodDelete, "/auth-app/tokens",
		url.Values{"token": {"hash-SECRET-HASH"}}, nil, nil, maxBody)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRET-HASH") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("transport error leaks a secret: %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	t.Cleanup(other.Close)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	})
	resp, err := c.do(context.Background(), Bearer(secretValue), http.MethodGet, "/x", nil, nil, nil, maxBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != http.StatusFound {
		t.Fatalf("status = %d, want the 302 itself", resp.status)
	}
	if otherHits.Load() != 0 {
		t.Fatal("the client followed a redirect")
	}
}

func TestBufferedResponsesAreCapped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), 100))
	})
	if _, err := c.do(context.Background(), Bearer(secretValue), http.MethodGet, "/x", nil, nil, nil, 99); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	resp, err := c.do(context.Background(), Bearer(secretValue), http.MethodGet, "/x", nil, nil, nil, 100)
	if err != nil || len(resp.body) != 100 {
		t.Fatalf("exact-size body: %v %d", err, len(resp.body))
	}
}

func TestErrorResponseBodiesAreNotBuffered(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal details that must not travel"))
	})
	resp, err := c.do(context.Background(), Bearer(secretValue), http.MethodGet, "/x", nil, nil, nil, maxBody)
	if err != nil {
		t.Fatal(err)
	}
	if resp.status != 500 || len(resp.body) != 0 {
		t.Fatalf("status %d body %q: error bodies must be dropped", resp.status, resp.body)
	}
}

func TestNewHTTPClientInsecureFlag(t *testing.T) {
	secure := NewHTTPClient(false).Transport.(*http.Transport)
	if secure.TLSClientConfig != nil && secure.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("verification must stay on by default")
	}
	insecure := NewHTTPClient(true).Transport.(*http.Transport)
	if insecure.TLSClientConfig == nil || !insecure.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("insecure=true must skip verification (dev stack only)")
	}
}

func TestMe(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graph/v1.0/me" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"4c51","displayName":"Albert Einstein","onPremisesSamAccountName":"einstein","mail":"e@example.org"}`))
	})
	u, err := c.Me(context.Background(), Bearer(secretValue))
	if err != nil {
		t.Fatal(err)
	}
	if u != (User{ID: "4c51", Username: "einstein", DisplayName: "Albert Einstein"}) {
		t.Fatalf("user = %+v", u)
	}
}

func TestMeRejectsUnauthorizedAndEmptyAnswers(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := c.Me(context.Background(), Bearer("bad")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if _, err := c.Me(context.Background(), Bearer("good")); err == nil {
		t.Fatal("an answer without an id must be an error")
	}
}

func TestPersonalDriveID(t *testing.T) {
	body := `{"value":[
	  {"id":"virt$virt","driveType":"virtual"},
	  {"id":"proj$space","driveType":"project"},
	  {"id":"6cb9$4c51","driveType":"personal"}]}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
	id, err := c.PersonalDriveID(context.Background(), Bearer(secretValue))
	if err != nil || id != "6cb9$4c51" {
		t.Fatalf("id = %q err = %v", id, err)
	}

	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[{"id":"x","driveType":"project"}]}`))
	})
	if _, err := c2.PersonalDriveID(context.Background(), Bearer(secretValue)); !errors.Is(err, ErrNoPersonalDrive) {
		t.Fatalf("err = %v, want ErrNoPersonalDrive", err)
	}
}
