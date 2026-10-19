package api

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// the real service is the API's credentials implementation
var _ Credentials = (*auth.Service)(nil)

type endpoint struct {
	method, path, body string
	stateChanging      bool
}

var tokenEndpoints = []endpoint{
	{http.MethodGet, "/agent-api/v1/status", "", false},
	{http.MethodPost, "/agent-api/v1/enroll", "{}", true},
	{http.MethodPost, "/agent-api/v1/unenroll", "{}", true},
	{http.MethodDelete, "/agent-api/v1/token", "", true},
	{http.MethodGet, "/agent-api/v1/token/revoke-all/preview", "", false},
	{http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`, true},
	{http.MethodPost, "/agent-api/v1/token/renew", "", true},
}

func TestTokenEndpointsNeedABearerAndStateChangesAnOrigin(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	before := len(s.fake.Tokens())
	s.fake.ResetRequests()
	for _, e := range tokenEndpoints {
		r := s.req(e.method, e.path, e.body)
		r.Header.Del("Authorization")
		wantError(t, s.serve(r), http.StatusUnauthorized, "unauthenticated")
		if e.stateChanging {
			r := s.req(e.method, e.path, e.body)
			r.Header.Del("Origin")
			wantError(t, s.serve(r), http.StatusForbidden, "origin_rejected")
			r = s.req(e.method, e.path, e.body)
			r.Header.Set("Origin", "https://evil.example.com")
			wantError(t, s.serve(r), http.StatusForbidden, "origin_rejected")
		}
	}
	if n := len(s.fake.Requests()); n != 0 {
		t.Fatalf("refused requests reached oCIS %d times", n)
	}
	if after := len(s.fake.Tokens()); after != before {
		t.Fatalf("tokens %d -> %d", before, after)
	}
	if _, ok := s.user(t, einsteinID); !ok {
		t.Fatal("a refused request changed the enrolment")
	}
}

func TestEnrollCreatesUserPolicyAndToken(t *testing.T) {
	s := newStack(t)
	rec := s.do(http.MethodPost, "/agent-api/v1/enroll", "{}")
	if rec.Code != http.StatusCreated {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body.String())
	}
	got := statusDoc(t, rec)
	if !got.Enrolled || got.Token.Status != "valid" || got.User.ID != einsteinID || got.User.Login != "einstein" || got.User.Name != "Einstein" {
		t.Fatalf("enroll body = %+v", got)
	}
	u, ok := s.user(t, einsteinID)
	if !ok || u.Name != "einstein" || u.PersonalDriveID != einsteinDrive {
		t.Fatalf("user = %+v %v", u, ok)
	}
	var pol store.Policy
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		pol, err = tx.GetPolicy(einsteinID)
		return err
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if pol.Paused || pol.DailyTokens != 0 { // 0: the operator's cap
		t.Fatalf("policy = %+v", pol)
	}
	if tok := s.appToken(t, einsteinID); tok.Status != store.TokenValid || len(tok.Sealed.Ciphertext) == 0 {
		t.Fatalf("token = %+v", tok.Status)
	}
	if n := s.ours("einstein"); n != 1 {
		t.Fatalf("live tokens of ours = %d", n)
	}
	if _, _, ok := s.fake.File(einsteinDrive, "/.ocis-agent/README.md"); !ok {
		t.Fatal("the memory folder was not prepared")
	}
	body := rec.Body.String()
	for _, bad := range []string{"apptoken-", "hash-of", "ocis-agent-", "oidc."} {
		if strings.Contains(body, bad) || strings.Contains(s.logs.String(), bad) {
			t.Errorf("token material %q in the response or logs", bad)
		}
	}
}

func TestEnrollTwiceReplacesTheToken(t *testing.T) {
	s := newStack(t)
	if rec := s.do(http.MethodPost, "/agent-api/v1/enroll", "{}"); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d", rec.Code)
	}
	first := s.appToken(t, einsteinID).Label
	rec := s.do(http.MethodPost, "/agent-api/v1/enroll", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("second = %d %s", rec.Code, rec.Body.String())
	}
	if got := statusDoc(t, rec); !got.Enrolled || got.Token.Status != "valid" {
		t.Fatalf("second body = %+v", got)
	}
	if again := s.appToken(t, einsteinID).Label; again == first {
		t.Fatal("re-enrolment kept the old token")
	}
	if labels := s.liveTokens("einstein"); len(labels) != 1 || labels[0] == first {
		t.Fatalf("live tokens = %v (the old one must be deleted)", labels)
	}
}

func TestEnrollNeedsAnEmptyBodyOrAnEmptyObject(t *testing.T) {
	s := newStack(t)
	for _, body := range []string{"null", "[]", `{"x":1}`, `{"user_id":"u-marie"}`, `{} {}`, `"{}"`, `{}x`} {
		r := s.req(http.MethodPost, "/agent-api/v1/enroll", body)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		wantError(t, s.serve(r), http.StatusBadRequest, "invalid_request")
	}
	if _, ok := s.user(t, einsteinID); ok {
		t.Fatal("a refused enrolment created the user")
	}
	if n := len(s.fake.Tokens()); n != 0 {
		t.Fatalf("a refused enrolment created %d tokens", n)
	}
}

func TestEnrollUpstreamFailures(t *testing.T) {
	s := newStack(t)
	s.fake.Fail(http.MethodPost, "/auth-app/tokens", http.StatusForbidden, 1)
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/enroll", "{}"), http.StatusBadGateway, "upstream_rejected")
	s.fake.Fail(http.MethodPost, "/auth-app/tokens", http.StatusServiceUnavailable, 1)
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/enroll", "{}"), http.StatusBadGateway, "upstream_unavailable")
	if got := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")); got.Token.Status == "valid" {
		t.Fatalf("a failed enrolment left a valid token: %+v", got)
	}
}

func TestActionBodiesMayBeLeftOut(t *testing.T) {
	for _, body := range []string{"", "{}"} {
		s := newStack(t)
		if rec := s.do(http.MethodPost, "/agent-api/v1/enroll", body); rec.Code != http.StatusCreated {
			t.Fatalf("enroll %q = %d %s", body, rec.Code, rec.Body.String())
		}
		if rec := s.do(http.MethodPost, "/agent-api/v1/token/renew", body); rec.Code != http.StatusOK {
			t.Fatalf("renew %q = %d %s", body, rec.Code, rec.Body.String())
		}
		if rec := s.do(http.MethodPost, "/agent-api/v1/unenroll", body); rec.Code != http.StatusNoContent {
			t.Fatalf("unenroll %q = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	s := newStack(t)
	s.enroll(t, "einstein")
	for _, body := range []string{"null", "[]", `{"x":1}`, `{"delete_memory":"yes"}`, `{"Delete_Memory":true}`, `{} {}`} {
		wantError(t, s.do(http.MethodPost, "/agent-api/v1/unenroll", body), http.StatusBadRequest, "invalid_request")
	}
	if _, ok := s.user(t, einsteinID); !ok {
		t.Fatal("a refused unenroll removed the user")
	}
}

func TestUnenrollKeepsOrDeletesTheMemory(t *testing.T) {
	for _, c := range []struct {
		body       string
		memoryGone bool
	}{{"{}", false}, {`{"delete_memory":false}`, false}, {`{"delete_memory":true}`, true}} {
		t.Run(c.body, func(t *testing.T) {
			s := newStack(t)
			s.enroll(t, "einstein")
			s.enroll(t, "marie")
			s.appPassword(t, "einstein", "my-phone")
			rec := s.do(http.MethodPost, "/agent-api/v1/unenroll", c.body)
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("unenroll = %d %q", rec.Code, rec.Body.String())
			}
			if _, ok := s.user(t, einsteinID); ok {
				t.Fatal("the user is still enrolled")
			}
			if n := s.ours("einstein"); n != 0 {
				t.Fatalf("%d of our tokens are left", n)
			}
			if l := s.liveTokens("einstein"); len(l) != 1 || l[0] != "my-phone" {
				t.Fatalf("the user's own app password must stay: %v", l)
			}
			_, _, kept := s.fake.File(einsteinDrive, "/.ocis-agent/README.md")
			if kept == c.memoryGone {
				t.Fatalf("memory kept = %v, want %v", kept, !c.memoryGone)
			}
			// marie is untouched
			if _, ok := s.user(t, marieID); !ok || s.ours("marie") != 1 {
				t.Fatal("unenrolling einstein touched marie")
			}
			if _, _, ok := s.fake.File(marieDrive, "/.ocis-agent/README.md"); !ok {
				t.Fatal("marie's memory is gone")
			}
			if got := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")); got.Enrolled {
				t.Fatal("status still enrolled")
			}
		})
	}
}

func TestUnenrollRefusesBadBodiesAndUnknownUsers(t *testing.T) {
	s := newStack(t)
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/unenroll", "{}"), http.StatusNotFound, "not_found")
	s.enroll(t, "einstein")
	for _, body := range []string{"null", `{"delete_memory":"true"}`, `{"delete_memory":null}`, `{"delete_memory":1}`,
		`{"Delete_Memory":true}`, `{"delete_memory":true,"x":1}`, `{"delete_memory":true,"delete_memory":false}`} {
		r := s.req(http.MethodPost, "/agent-api/v1/unenroll", body)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		wantError(t, s.serve(r), http.StatusBadRequest, "invalid_request")
	}
	if _, ok := s.user(t, einsteinID); !ok || s.ours("einstein") != 1 {
		t.Fatal("a refused unenroll changed something")
	}
}

func TestRevokeDeletesOurTokensOnly(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	s.appPassword(t, "einstein", "my-phone")
	rec := s.do(http.MethodDelete, "/agent-api/v1/token", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	if m := decode(t, rec); !reflect.DeepEqual(m, map[string]any{"revoked": float64(1)}) {
		t.Fatalf("revoke body = %v", m)
	}
	if l := s.liveTokens("einstein"); len(l) != 1 || l[0] != "my-phone" {
		t.Fatalf("live tokens = %v", l)
	}
	if s.ours("marie") != 1 {
		t.Fatal("marie's token was revoked")
	}
	got := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", ""))
	if got.Token.Status != "revoked" || !got.Paused || got.PauseReason != store.PauseCredentialRevoked {
		t.Fatalf("after revoke = %+v", got)
	}
	// a DELETE with a body is refused before anything happens
	r := s.req(http.MethodDelete, "/agent-api/v1/token", "{}")
	r.Header.Set("Content-Type", "application/json")
	wantError(t, s.serve(r), http.StatusBadRequest, "invalid_request")
}

func TestRevokeAllPreviewListsLabelsOnly(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.fake.Advance(1)
	s.appPassword(t, "einstein", "my-phone")
	s.appPassword(t, "marie", "marie-phone")
	rec := s.do(http.MethodGet, "/agent-api/v1/token/revoke-all/preview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if len(m) != 1 {
		t.Fatalf("preview keys = %v", m)
	}
	list := m["tokens"].([]any)
	if len(list) != 2 {
		t.Fatalf("preview = %v", list)
	}
	ours := s.appToken(t, einsteinID).Label
	wantLabels := []string{ours, "my-phone"}
	for i, it := range list {
		e := it.(map[string]any)
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"created_at", "expires_at", "label", "ours"}) {
			t.Fatalf("entry keys = %v", keys)
		}
		if e["label"] != wantLabels[i] || e["ours"] != (i == 0) {
			t.Errorf("entry %d = %v", i, e)
		}
	}
	body := rec.Body.String()
	for _, bad := range []string{"apptoken-", "hash-of", "marie-phone"} {
		if strings.Contains(body, bad) {
			t.Errorf("preview leaks %q", bad)
		}
	}
	if len(s.liveTokens("einstein")) != 2 {
		t.Fatal("the preview deleted something")
	}
}

func TestRevokeAllNeedsConfirmTrue(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.appPassword(t, "einstein", "my-phone")
	s.fake.ResetRequests()
	for _, body := range []string{"", "{}", "null", `{"confirm":false}`, `{"confirm":null}`, `{"confirm":"true"}`, `{"confirm":1}`,
		`{"Confirm":true}`, `{"confirm":true,"x":1}`, `{"confirm":false,"confirm":true}`, `{"confirm":[true]}`} {
		r := s.req(http.MethodPost, "/agent-api/v1/token/revoke-all", body)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		wantError(t, s.serve(r), http.StatusBadRequest, "invalid_request")
	}
	if n := len(s.fake.Requests()); n != 0 {
		t.Fatalf("an unconfirmed revoke-all sent %d requests to oCIS", n)
	}
	if len(s.liveTokens("einstein")) != 2 {
		t.Fatal("an unconfirmed revoke-all deleted tokens")
	}
	s.enroll(t, "marie")
	rec := s.do(http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body.String())
	}
	if m := decode(t, rec); !reflect.DeepEqual(m, map[string]any{"revoked": float64(2)}) {
		t.Fatalf("revoke-all body = %v", m)
	}
	if l := s.liveTokens("einstein"); len(l) != 0 {
		t.Fatalf("left: %v", l)
	}
	if s.ours("marie") != 1 {
		t.Fatal("revoke-all of einstein touched marie")
	}
	if got := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")); got.PauseReason != store.PauseCredentialRevoked {
		t.Fatalf("after revoke-all = %+v", got)
	}
}

func TestRenew(t *testing.T) {
	s := newStack(t)
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/token/renew", ""), http.StatusConflict, "credentials_invalid")
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	old := s.appToken(t, einsteinID).Label
	rec := s.do(http.MethodPost, "/agent-api/v1/token/renew", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("renew = %d %s", rec.Code, rec.Body.String())
	}
	if m := decode(t, rec); !reflect.DeepEqual(m, map[string]any{"renewed": true, "old_cleaned": true}) {
		t.Fatalf("renew body = %v", m)
	}
	now := s.appToken(t, einsteinID).Label
	if now == old {
		t.Fatal("the token was not replaced")
	}
	if l := s.liveTokens("einstein"); len(l) != 1 || l[0] != now {
		t.Fatalf("live = %v", l)
	}
	if strings.Contains(rec.Body.String(), "ocis-agent-") {
		t.Fatal("renew leaks the label")
	}
	if s.ours("marie") != 1 {
		t.Fatal("renewing einstein touched marie")
	}
	// anything but an empty body or {} is refused, nothing renewed
	for _, body := range []string{"null", "[]", `{"x":1}`, `{"user_id":"u-marie"}`, `{} {}`, `"{}"`, `{}x`} {
		wantError(t, s.do(http.MethodPost, "/agent-api/v1/token/renew", body), http.StatusBadRequest, "invalid_request")
	}
	if s.appToken(t, einsteinID).Label != now {
		t.Fatal("a refused renew changed the token")
	}
	// after a revoke there is nothing to renew: enrol again
	if rec := s.do(http.MethodDelete, "/agent-api/v1/token", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/token/renew", ""), http.StatusConflict, "credentials_invalid")
}

func TestRenewWithADeadTokenIsCredentialsInvalid(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	// the token is deleted behind the agent's back
	rec := s.do(http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	wantError(t, s.do(http.MethodPost, "/agent-api/v1/token/renew", ""), http.StatusConflict, "credentials_invalid")
}

// TestTheBearerNeverLeaks drives every endpoint and checks that neither the
// bearer, nor an app token, nor a token hash appears in a response or a log
// line, and labels only in the revoke-all preview.
func TestTheBearerNeverLeaks(t *testing.T) {
	s := newStack(t)
	bearer := fakeocis.OIDCToken("einstein")
	var bodies []string
	call := func(method, path, body string, want int) {
		t.Helper()
		rec := s.do(method, path, body)
		if rec.Code != want {
			t.Errorf("%s %s = %d, want %d", method, path, rec.Code, want)
		}
		b := rec.Body.String()
		for k, v := range rec.Header() {
			b += k + strings.Join(v, ",")
		}
		if path != "/agent-api/v1/token/revoke-all/preview" {
			bodies = append(bodies, b)
		} else if strings.Contains(b, bearer) || strings.Contains(b, "apptoken-") || strings.Contains(b, "hash-of") {
			t.Errorf("preview leaks token material: %s", b)
		}
	}
	call(http.MethodGet, "/agent-api/v1/status", "", 200)
	call(http.MethodPost, "/agent-api/v1/enroll", "{}", 201)
	call(http.MethodPost, "/agent-api/v1/enroll", "{}", 200)
	call(http.MethodGet, "/agent-api/v1/status", "", 200)
	call(http.MethodPost, "/agent-api/v1/token/renew", "", 200)
	call(http.MethodGet, "/agent-api/v1/token/revoke-all/preview", "", 200)
	call(http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":false}`, 400)
	call(http.MethodDelete, "/agent-api/v1/token", "", 200)
	call(http.MethodPost, "/agent-api/v1/enroll", "{}", 200)
	call(http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`, 200)
	call(http.MethodPost, "/agent-api/v1/enroll", "{}", 200)
	// failures carry no token material either
	s.fake.Fail(http.MethodPost, "/auth-app/tokens", http.StatusInternalServerError, 1)
	call(http.MethodPost, "/agent-api/v1/token/renew", "", 502)
	s.fake.Fail(http.MethodGet, "/auth-app/tokens", http.StatusInternalServerError, 1)
	call(http.MethodDelete, "/agent-api/v1/token", "", 502)
	call(http.MethodPost, "/agent-api/v1/unenroll", `{"delete_memory":true}`, 204)
	logs := s.logs.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, bad := range []string{bearer, "apptoken-", "hash-of", "ocis-agent-"} {
		if strings.Contains(logs, bad) {
			t.Errorf("logs contain %q:\n%s", bad, logs)
		}
		for _, b := range bodies {
			if strings.Contains(b, bad) {
				t.Errorf("a response contains %q: %s", bad, b)
			}
		}
	}
}

func TestRevokeByAUserWhoNeverEnrolledLeavesNoAuditRow(t *testing.T) {
	s := newStack(t)
	rec := s.as("einstein", http.MethodDelete, "/agent-api/v1/token", "")
	if rec.Code != 200 || rec.Body.String() != `{"revoked":0}`+"\n" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if evs := s.auditOf(t, einsteinID); len(evs) != 0 {
		t.Errorf("audit = %+v", evs)
	}
}

// The actions that call oCIS (enrol, renew, revoke, revoke-all) share a
// per-user cooldown; a request refused before the action does not use it.
func TestOCISBoundActionsHaveAPerUserCooldown(t *testing.T) {
	s := newStack(t)
	s.srv.actions.every = actionCooldown
	s.enroll(t, "einstein")
	rec := s.as("einstein", http.MethodPost, "/agent-api/v1/token/renew", "")
	wantError(t, rec, 429, "rate_limited")
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q", ra)
	}
	wantError(t, s.as("einstein", http.MethodDelete, "/agent-api/v1/token", ""), 429, "rate_limited")
	wantError(t, s.as("einstein", http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`), 429, "rate_limited")
	wantError(t, s.as("einstein", http.MethodPost, "/agent-api/v1/enroll", ""), 429, "rate_limited")
	s.enroll(t, "marie") // her own cooldown

	s.fake.Advance(actionCooldown)
	if rec := s.as("einstein", http.MethodPost, "/agent-api/v1/token/renew", ""); rec.Code != 200 {
		t.Fatalf("renew after the cooldown: %d %s", rec.Code, rec.Body.String())
	}
	s.fake.Advance(actionCooldown)
	// a refused body does not start the cooldown
	wantError(t, s.as("einstein", http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":false}`), 400, "invalid_request")
	if rec := s.as("einstein", http.MethodPost, "/agent-api/v1/token/revoke-all", `{"confirm":true}`); rec.Code != 200 {
		t.Errorf("revoke-all: %d %s", rec.Code, rec.Body.String())
	}
	wantError(t, s.as("einstein", http.MethodDelete, "/agent-api/v1/token", ""), 429, "rate_limited")
}

func TestCooldownIsBounded(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	cd := newCooldown(5*time.Second, 3, c.Now)
	for i := range 10 {
		cd.take(fmt.Sprintf("u%d", i))
		if n := cd.size(); n > 3 {
			t.Fatalf("size = %d", n)
		}
	}
	if ok, wait := cd.take("u0"); !ok && wait <= 0 {
		t.Errorf("take = %v %v", ok, wait)
	}
	c.Advance(6 * time.Second)
	if ok, _ := cd.take("u1"); !ok {
		t.Error("cooldown did not end")
	}
	if ok, wait := cd.take("u1"); ok || wait != 5*time.Second {
		t.Errorf("second take = %v %v", ok, wait)
	}
	off := newCooldown(0, 3, c.Now)
	for range 3 {
		if ok, _ := off.take("u"); !ok {
			t.Error("a zero cooldown refused")
		}
	}
}
