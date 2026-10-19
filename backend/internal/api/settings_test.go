package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	pausePath    = "/agent-api/v1/pause"
	resumePath   = "/agent-api/v1/resume"
	settingsPath = "/agent-api/v1/settings"
)

func (s *stack) policy(t *testing.T, id string) store.Policy {
	t.Helper()
	var p store.Policy
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		p, err = tx.GetPolicy(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *stack) setPolicy(t *testing.T, id string, mod func(*store.Policy)) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		p, err := tx.GetPolicy(id)
		if err != nil {
			return err
		}
		mod(&p)
		return tx.PutPolicy(p)
	})
}

func (s *stack) auditOf(t *testing.T, id string) []audit.Record {
	t.Helper()
	var out []audit.Record
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		out, err = tx.ReadAudit(id, 0, 1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *stack) auditKind(t *testing.T, id, kind string) []audit.Record {
	t.Helper()
	var out []audit.Record
	for _, r := range s.auditOf(t, id) {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (s *stack) wakes() int {
	s.wk.mu.Lock()
	defer s.wk.mu.Unlock()
	return s.wk.n
}

func TestPauseSetsTheUserPauseAndAudits(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	for _, body := range []string{"{}", ""} {
		rec := s.do(http.MethodPost, pausePath, body)
		v := statusDoc(t, rec)
		if !v.Enrolled || !v.Paused || v.PauseReason != store.PauseUser {
			t.Fatalf("body %q: status = %+v", body, v)
		}
	}
	if p := s.policy(t, einsteinID); !p.Paused || p.PauseReason != store.PauseUser {
		t.Fatalf("policy = %+v", p)
	}
	// idempotent: the second pause changed nothing and wrote no second event
	ev := s.auditKind(t, einsteinID, "agent_paused")
	if len(ev) != 1 || ev[0].Cred != "oidc" || ev[0].Detail != "" {
		t.Fatalf("audit = %+v", ev)
	}
}

func (s *stack) pauseFor(t *testing.T, reason string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.PausePolicy(einsteinID, reason)
		return err
	})
}

var strongPauses = []string{store.PauseCredentialInvalid, store.PauseCredentialRevoked, store.PauseCredentialExpired, store.PauseTokenAnomaly}

func TestPauseKeepsACredentialOrAnomalyPause(t *testing.T) {
	for _, reason := range strongPauses {
		t.Run(reason, func(t *testing.T) {
			s := newStack(t)
			s.enroll(t, "einstein")
			s.pauseFor(t, reason)
			before := s.policy(t, einsteinID)
			if v := statusDoc(t, s.do(http.MethodPost, pausePath, "{}")); !v.Paused || v.PauseReason != reason {
				t.Fatalf("status = %+v", v)
			}
			if after := s.policy(t, einsteinID); !reflect.DeepEqual(after, before) {
				t.Fatalf("policy changed: %+v -> %+v", before, after)
			}
			ev := s.auditKind(t, einsteinID, "agent_paused")
			if len(ev) != 1 || ev[0].Outcome != "kept_pause" || ev[0].Detail != "reason="+reason || ev[0].Cred != "oidc" {
				t.Fatalf("audit = %+v", ev)
			}
		})
	}
}

func TestPauseReplacesDormantAndTheUserPauseIsSticky(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.pauseFor(t, store.PauseDormant)
	if v := statusDoc(t, s.do(http.MethodPost, pausePath, "{}")); v.PauseReason != store.PauseUser {
		t.Fatalf("reason = %q", v.PauseReason)
	}
	// an automatic reason never replaces the user's pause; repeating it is idempotent
	s.pauseFor(t, store.PauseCredentialRevoked)
	if v := statusDoc(t, s.do(http.MethodPost, pausePath, "{}")); v.PauseReason != store.PauseUser {
		t.Fatalf("reason = %q", v.PauseReason)
	}
	ev := s.auditKind(t, einsteinID, "agent_paused")
	if len(ev) != 1 || ev[0].Outcome != "paused" {
		t.Fatalf("events = %+v, want the one real pause (the repeat changed nothing)", ev)
	}
}

// The regression: pause then resume(user) must not be a way around the
// resume allowlist.
func TestPauseThenResumeCannotClearAStrongPause(t *testing.T) {
	for _, reason := range strongPauses {
		t.Run(reason, func(t *testing.T) {
			s := newStack(t)
			s.enroll(t, "einstein")
			s.pauseFor(t, reason)
			before := s.policy(t, einsteinID)
			statusDoc(t, s.do(http.MethodPost, pausePath, "{}"))
			v := statusDoc(t, s.do(http.MethodPost, resumePath, `{"reason":"user"}`))
			if !v.Paused || v.PauseReason != reason {
				t.Fatalf("status = %+v", v)
			}
			if after := s.policy(t, einsteinID); !reflect.DeepEqual(after, before) {
				t.Fatalf("policy changed: %+v -> %+v", before, after)
			}
			if reason == store.PauseTokenAnomaly {
				v = statusDoc(t, s.do(http.MethodPost, resumePath, `{"reason":"token_anomaly"}`))
				if v.Paused {
					t.Fatalf("resume(token_anomaly) did not clear: %+v", v)
				}
			}
		})
	}
}

func TestPauseBodyAndEnrolment(t *testing.T) {
	s := newStack(t)
	wantError(t, s.do(http.MethodPost, pausePath, "{}"), http.StatusNotFound, "not_found")
	s.enroll(t, "einstein")
	for _, body := range []string{`{"reason":"user"}`, `[]`, `null`, `{`, `{"x":1}`} {
		wantError(t, s.do(http.MethodPost, pausePath, body), http.StatusBadRequest, "invalid_request")
	}
	if p := s.policy(t, einsteinID); p.Paused {
		t.Fatal("a refused request paused the agent")
	}
}

func TestPauseIsScopedToTheCaller(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	r := s.req(http.MethodPost, pausePath+"?user_id="+marieID, "{}")
	r.Header.Set("X-User", marieID)
	s.serve(r)
	if !s.policy(t, einsteinID).Paused || s.policy(t, marieID).Paused {
		t.Fatal("pause touched another user")
	}
}

func TestResumeClearsUserAndTokenAnomalyPauses(t *testing.T) {
	for _, reason := range []string{store.PauseUser, store.PauseTokenAnomaly} {
		for _, body := range []string{"", "{}", `{"reason":"` + reason + `"}`} {
			if body == "" || body == "{}" {
				if reason != store.PauseUser {
					continue // the default reason is user
				}
			}
			t.Run(reason+"/"+body, func(t *testing.T) {
				s := newStack(t)
				s.enroll(t, "einstein")
				s.mustTx(t, func(tx store.Tx) error {
					_, err := tx.PausePolicy(einsteinID, reason)
					return err
				})
				before := s.wakes()
				v := statusDoc(t, s.do(http.MethodPost, resumePath, body))
				if v.Paused || v.PauseReason != "" {
					t.Fatalf("status = %+v", v)
				}
				if p := s.policy(t, einsteinID); p.Paused || p.PauseReason != "" {
					t.Fatalf("policy = %+v", p)
				}
				ev := s.auditKind(t, einsteinID, "agent_resumed")
				if len(ev) != 1 || ev[0].Cred != "oidc" || ev[0].Detail != "reason="+reason {
					t.Fatalf("audit = %+v", ev)
				}
				if s.wakes() != before+1 {
					t.Fatalf("wakes = %d, want %d", s.wakes(), before+1)
				}
			})
		}
	}
}

func TestResumeRefusesOtherReasons(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.PausePolicy(einsteinID, store.PauseCredentialInvalid)
		return err
	})
	before := s.wakes()
	for _, reason := range []string{"credential_invalid", "credential_revoked", "credential_expired", "dormant", "nope", "", "User", " user"} {
		wantError(t, s.do(http.MethodPost, resumePath, fmt.Sprintf(`{"reason":%q}`, reason)), http.StatusBadRequest, "invalid_request")
	}
	for _, body := range []string{`{"reason":1}`, `{"reason":null}`, `{"reason":["user"]}`, `{"Reason":"user"}`, `{"reason":"user","x":1}`, `[]`} {
		wantError(t, s.do(http.MethodPost, resumePath, body), http.StatusBadRequest, "invalid_request")
	}
	if p := s.policy(t, einsteinID); !p.Paused || p.PauseReason != store.PauseCredentialInvalid {
		t.Fatalf("policy = %+v", p)
	}
	if s.wakes() != before || len(s.auditKind(t, einsteinID, "agent_resumed")) != 0 {
		t.Fatal("a refused resume woke the queue or audited")
	}
}

func TestResumeOfAnotherReasonIsANoOp(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	// not paused at all
	v := statusDoc(t, s.do(http.MethodPost, resumePath, ""))
	if v.Paused {
		t.Fatal("paused")
	}
	// paused for a credential reason: resume(user) leaves it
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.PausePolicy(einsteinID, store.PauseCredentialRevoked)
		return err
	})
	v = statusDoc(t, s.do(http.MethodPost, resumePath, `{"reason":"user"}`))
	if !v.Paused || v.PauseReason != store.PauseCredentialRevoked {
		t.Fatalf("status = %+v", v)
	}
	if n := len(s.auditKind(t, einsteinID, "agent_resumed")); n != 0 {
		t.Fatalf("%d agent_resumed events for a no-op", n)
	}
}

func TestResumeNotEnrolled(t *testing.T) {
	s := newStack(t)
	wantError(t, s.do(http.MethodPost, resumePath, ""), http.StatusNotFound, "not_found")
}

func putSettings(s *stack, body string) *httptest.ResponseRecorder {
	return s.do(http.MethodPut, settingsPath, body)
}

func settingsDoc(t *testing.T, rec *httptest.ResponseRecorder) settingsView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var v settingsView
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("settings body %q: %v", rec.Body.String(), err)
	}
	return v
}

func mustOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }

func TestSettingsDefaults(t *testing.T) {
	s := newStack(t)
	wantError(t, s.do(http.MethodGet, settingsPath, ""), http.StatusNotFound, "not_found")
	s.enroll(t, "einstein")
	rec := s.do(http.MethodGet, settingsPath, "")
	got := settingsDoc(t, rec)
	want := settingsView{
		Watch: []string{}, SweepInterval: "6h0m0s", DailyTokens: 200000, DailyTokensMax: 200000, DormancyDays: 30,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	m := decode(t, rec)
	for _, k := range []string{"proactive_enabled", "watch", "sweep_interval", "quiet_hours", "daily_tokens", "daily_tokens_max", "dormancy_days", "models"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing in %s", k, rec.Body.String())
		}
	}
	models := m["models"].(map[string]any)
	if v, ok := models["chat"]; !ok || v != nil {
		t.Errorf("chat = %v (present %v), want null", v, ok)
	}
	if v, ok := models["proactive"]; !ok || v != nil {
		t.Errorf("proactive = %v (present %v), want null", v, ok)
	}
	if w, ok := m["watch"].([]any); !ok || len(w) != 0 {
		t.Errorf("watch = %v, want []", m["watch"])
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	body := `{"proactive_enabled":true,"watch":["/Projects","/Docs/Notes"],"sweep_interval":"30m","quiet_hours":"22:00-07:00",` +
		`"daily_tokens":5000,"models":{"chat":"model-b","proactive":"model-a"}}`
	got := settingsDoc(t, putSettings(s, body))
	want := settingsView{
		ProactiveEnabled: true, Watch: []string{"/Projects", "/Docs/Notes"}, SweepInterval: "30m0s", QuietHours: "22:00-07:00",
		DailyTokens: 5000, DailyTokensMax: 200000, DormancyDays: 30,
		Models: settingsModels{Chat: ptr("model-b"), Proactive: ptr("model-a")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PUT = %+v, want %+v", got, want)
	}
	if again := settingsDoc(t, s.do(http.MethodGet, settingsPath, "")); !reflect.DeepEqual(again, want) {
		t.Fatalf("GET = %+v, want %+v", again, want)
	}
	p := s.policy(t, einsteinID)
	if !p.ProactiveEnabled || p.SweepInterval != 30*time.Minute || p.DailyTokens != 5000 || p.QuietHours != "22:00-07:00" ||
		p.Models[store.ModelTaskChat] != "model-b" || p.Models[store.ModelTaskProactive] != "model-a" ||
		!reflect.DeepEqual(p.Watch, []string{"/Projects", "/Docs/Notes"}) {
		t.Fatalf("policy = %+v", p)
	}
}

func TestSettingsWatchIsNormalisedAndDeduplicated(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	got := settingsDoc(t, putSettings(s, `{"watch":["/a/","/a","/b//c","/a"]}`))
	if want := []string{"/a", "/b/c"}; !reflect.DeepEqual(got.Watch, want) {
		t.Fatalf("watch = %v, want %v", got.Watch, want)
	}
	if got := settingsDoc(t, putSettings(s, `{"watch":[]}`)); len(got.Watch) != 0 || got.Watch == nil {
		t.Fatalf("watch = %#v, want empty non-nil", got.Watch)
	}
}

func TestSettingsPutPreservesWhatItDoesNotMention(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.setPolicy(t, einsteinID, func(p *store.Policy) {
		p.AutoApproveWrite = true
		p.DormancyDays = 7
		p.Watch = []string{"/keep"}
		p.QuietHours = "01:00-02:00"
		p.Models = map[string]string{store.ModelTaskChat: "model-b"}
	})
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.PausePolicy(einsteinID, store.PauseTokenAnomaly)
		return err
	})
	before := s.policy(t, einsteinID)
	mustOK(t, putSettings(s, `{"proactive_enabled":true}`))
	after := s.policy(t, einsteinID)
	if !after.AutoApproveWrite || after.DormancyDays != 7 || !after.Paused || after.PauseReason != store.PauseTokenAnomaly ||
		!reflect.DeepEqual(after.Watch, []string{"/keep"}) || after.QuietHours != "01:00-02:00" ||
		after.Models[store.ModelTaskChat] != "model-b" || after.SweepInterval != before.SweepInterval || after.DailyTokens != before.DailyTokens {
		t.Fatalf("after = %+v, before = %+v", after, before)
	}
	if !after.ProactiveEnabled {
		t.Fatal("proactive_enabled not applied")
	}
	// a second field in the next PUT keeps the first
	putSettings(s, `{"quiet_hours":""}`)
	after = s.policy(t, einsteinID)
	if !after.ProactiveEnabled || after.QuietHours != "" {
		t.Fatalf("after = %+v", after)
	}
}

func TestSettingsModelPreferenceSetAndClear(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	settingsDoc(t, putSettings(s, `{"models":{"chat":"model-b","proactive":"model-a"}}`))
	// only mention proactive: chat stays
	got := settingsDoc(t, putSettings(s, `{"models":{"proactive":null}}`))
	if got.Models.Chat == nil || *got.Models.Chat != "model-b" || got.Models.Proactive != nil {
		t.Fatalf("models = %+v", got.Models)
	}
	p := s.policy(t, einsteinID)
	if _, ok := p.Models[store.ModelTaskProactive]; ok || p.Models[store.ModelTaskChat] != "model-b" {
		t.Fatalf("stored models = %v", p.Models)
	}
	// clearing a key that is not set is fine
	got = settingsDoc(t, putSettings(s, `{"models":{"chat":null,"proactive":null}}`))
	if got.Models.Chat != nil || got.Models.Proactive != nil {
		t.Fatalf("models = %+v", got.Models)
	}
}

func TestSettingsReportsAStalePreferenceAsNull(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.setPolicy(t, einsteinID, func(p *store.Policy) {
		p.Models = map[string]string{store.ModelTaskChat: "removed-model", store.ModelTaskProactive: "model-a"}
	})
	got := settingsDoc(t, s.do(http.MethodGet, settingsPath, ""))
	if got.Models.Chat != nil || got.Models.Proactive == nil || *got.Models.Proactive != "model-a" {
		t.Fatalf("models = %+v", got.Models)
	}
	// a PUT of another field keeps the stale value in the database untouched
	putSettings(s, `{"proactive_enabled":true}`)
	if s.policy(t, einsteinID).Models[store.ModelTaskChat] != "removed-model" {
		t.Fatal("a PUT changed a preference it did not mention")
	}
}

func TestSettingsRejectsInvalidInput(t *testing.T) {
	watch21 := `"/p0"`
	for i := 1; i < 21; i++ {
		watch21 += fmt.Sprintf(`,"/p%d"`, i)
	}
	watch20 := strings.Replace(watch21, `,"/p20"`, "", 1)
	long := "/" + strings.Repeat("a", 1024) // 1025 bytes
	ok1024 := "/" + strings.Repeat("a", 1023)
	cases := []struct{ name, body string }{
		{"empty object", `{}`},
		{"watch 21 entries", `{"watch":[` + watch21 + `]}`},
		{"watch relative", `{"watch":["Projects"]}`},
		{"watch empty entry", `{"watch":[""]}`},
		{"watch dotdot", `{"watch":["/a/../b"]}`},
		{"watch dot", `{"watch":["/a/./b"]}`},
		{"watch reserved", `{"watch":["/.ocis-agent"]}`},
		{"watch reserved memory", `{"watch":["/.ocis-agent/memory/profile.md"]}`},
		{"watch reserved case", `{"watch":["/.OCIS-Agent/x"]}`},
		{"watch reserved unclean", `{"watch":["/x//..//.ocis-agent"]}`},
		{"watch control", `{"watch":["/a\u0001b"]}`},
		{"watch newline", `{"watch":["/a\nb"]}`},
		{"watch DEL", `{"watch":["/a\u007fb"]}`},
		{"watch C1", `{"watch":["/a\u0085b"]}`},
		{"watch line separator", `{"watch":["/a b"]}`},
		{"watch bidi", `{"watch":["/a‮b"]}`},
		{"watch LRM", `{"watch":["/a‎b"]}`},
		{"watch RLM", `{"watch":["/a‏b"]}`},
		{"watch ALM", `{"watch":["/a؜b"]}`},
		{"watch bidi isolate", `{"watch":["/a⁦b"]}`},
		{"watch too long", `{"watch":["` + long + `"]}`},
		{"watch null", `{"watch":null}`},
		{"watch not array", `{"watch":"/a"}`},
		{"watch number entry", `{"watch":[1]}`},
		{"watch null entry", `{"watch":[null]}`},
		{"interval 4m59s", `{"sweep_interval":"4m59s"}`},
		{"interval 7d1s", `{"sweep_interval":"168h1s"}`},
		{"interval fractional", `{"sweep_interval":"5m0.5s"}`},
		{"interval milliseconds", `{"sweep_interval":"3600500ms"}`},
		{"interval number", `{"sweep_interval":3600}`},
		{"interval garbage", `{"sweep_interval":"soon"}`},
		{"interval empty", `{"sweep_interval":""}`},
		{"interval negative", `{"sweep_interval":"-1h"}`},
		{"interval null", `{"sweep_interval":null}`},
		{"quiet 25:00", `{"quiet_hours":"25:00-07:00"}`},
		{"quiet 24:00", `{"quiet_hours":"24:00-07:00"}`},
		{"quiet minute 60", `{"quiet_hours":"22:60-07:00"}`},
		{"quiet no dash", `{"quiet_hours":"22:00"}`},
		{"quiet short", `{"quiet_hours":"2:00-7:00"}`},
		{"quiet spaces", `{"quiet_hours":" 22:00-07:00"}`},
		{"quiet equal", `{"quiet_hours":"07:00-07:00"}`},
		{"quiet number", `{"quiet_hours":2200}`},
		{"quiet null", `{"quiet_hours":null}`},
		{"tokens zero", `{"daily_tokens":0}`},
		{"tokens 999", `{"daily_tokens":999}`},
		{"tokens above cap", `{"daily_tokens":200001}`},
		{"tokens negative", `{"daily_tokens":-5000}`},
		{"tokens fractional", `{"daily_tokens":5000.5}`},
		{"tokens exponent", `{"daily_tokens":5e3}`},
		{"tokens string", `{"daily_tokens":"5000"}`},
		{"tokens huge", `{"daily_tokens":99999999999999999999}`},
		{"proactive string", `{"proactive_enabled":"true"}`},
		{"proactive null", `{"proactive_enabled":null}`},
		{"models not in catalog", `{"models":{"chat":"gpt-unknown"}}`},
		{"models invalid id", `{"models":{"chat":"bad id!"}}`},
		{"models empty id", `{"models":{"chat":""}}`},
		{"models number", `{"models":{"chat":5}}`},
		{"models bool", `{"models":{"chat":true}}`},
		{"models object", `{"models":{"chat":{}}}`},
		{"models unknown task", `{"models":{"summary":"model-a"}}`},
		{"models task case", `{"models":{"Chat":"model-a"}}`},
		{"models null", `{"models":null}`},
		{"models empty object", `{"models":{}}`},
		{"models array", `{"models":["model-a"]}`},
		{"models dup key", `{"models":{"chat":"model-a","chat":"model-b"}}`},
		{"unknown field", `{"auto_approve_write":true}`},
		{"unknown field with valid", `{"proactive_enabled":true,"auto_approve_write":true}`},
		{"paused", `{"paused":true}`},
		{"pause_reason", `{"pause_reason":"user"}`},
		{"dormancy_days", `{"dormancy_days":1}`},
		{"user_id", `{"user_id":"u-marie"}`},
		{"updated_at", `{"updated_at":"2026-01-01T00:00:00Z"}`},
		{"daily_tokens_max", `{"daily_tokens_max":5000}`},
		{"case variant", `{"Daily_Tokens":5000}`},
		{"case variant upper", `{"WATCH":["/a"]}`},
		{"case variant not editable", `{"Auto_Approve_Write":true}`},
		{"duplicate key", `{"daily_tokens":5000,"daily_tokens":6000}`},
		{"not an object", `[]`},
		{"null body", `null`},
		{"broken", `{"daily_tokens":`},
		{"trailing data", `{"daily_tokens":5000} {}`},
	}
	s := newStack(t)
	s.enroll(t, "einstein")
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.AutoApproveWrite = true })
	before := s.policy(t, einsteinID)
	nAudit := len(s.auditOf(t, einsteinID))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, putSettings(s, tc.body), http.StatusBadRequest, "invalid_request")
		})
	}
	if after := s.policy(t, einsteinID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused PUT changed the policy: %+v -> %+v", before, after)
	}
	if n := len(s.auditOf(t, einsteinID)); n != nAudit {
		t.Fatalf("refused PUTs wrote %d audit events", n-nAudit)
	}

	// boundaries that are accepted
	for _, body := range []string{
		`{"watch":[` + watch20 + `]}`,
		`{"watch":["` + ok1024 + `"]}`,
		`{"watch":["/.ocis-agentx","/a/.ocis-agent"]}`,
		`{"watch":["/Übung/日本語 ‍x"]}`,
		`{"sweep_interval":"5m"}`, `{"sweep_interval":"168h"}`, `{"sweep_interval":"1h30m15s"}`,
		`{"quiet_hours":"00:00-23:59"}`, `{"quiet_hours":""}`,
		`{"daily_tokens":1000}`, `{"daily_tokens":200000}`,
	} {
		if rec := putSettings(s, body); rec.Code != http.StatusOK {
			t.Errorf("%.80s: status = %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestSettingsTokenCapFollowsTheOperator(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.UserDailyTokens = 50000 })
	s.enroll(t, "einstein")
	wantError(t, putSettings(s, `{"daily_tokens":50001}`), http.StatusBadRequest, "invalid_request")
	got := settingsDoc(t, putSettings(s, `{"daily_tokens":50000}`))
	if got.DailyTokens != 50000 || got.DailyTokensMax != 50000 {
		t.Fatalf("settings = %+v", got)
	}
	// a stored value above the cap (the operator lowered it) shows the cap, and is never 0
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.DailyTokens = 0 })
	got = settingsDoc(t, s.do(http.MethodGet, settingsPath, ""))
	if got.DailyTokens != 50000 {
		t.Fatalf("daily_tokens = %d for a stored 0, want the cap", got.DailyTokens)
	}
	settingsDoc(t, putSettings(s, `{"daily_tokens":1000}`))
	if p := s.policy(t, einsteinID); p.DailyTokens != 1000 {
		t.Fatalf("stored = %d", p.DailyTokens)
	}
}

func TestSettingsAuditListsNamesOnly(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	putSettings(s, `{"watch":["/SECRET-PATH"],"daily_tokens":4321,"models":{"chat":"model-b"},"quiet_hours":"11:11-12:12"}`)
	ev := s.auditKind(t, einsteinID, "policy_changed")
	if len(ev) != 1 {
		t.Fatalf("events = %+v", ev)
	}
	if ev[0].Detail != "daily_tokens,models,quiet_hours,watch" || ev[0].Cred != "oidc" {
		t.Fatalf("detail = %q cred = %q", ev[0].Detail, ev[0].Cred)
	}
	for _, bad := range []string{"SECRET", "4321", "model-b", "11:11"} {
		// the event's fields only: the record's PrevHash and Hash are random
		// hex and contain "4321" by chance in ~1 of 250 runs
		if strings.Contains(fmt.Sprintf("%+v", ev[0].Event), bad) {
			t.Errorf("audit leaks %q: %+v", bad, ev[0].Event)
		}
	}
	// only fields that really change are listed; no change, no write, no event
	putSettings(s, `{"watch":["/SECRET-PATH"],"proactive_enabled":true}`)
	ev = s.auditKind(t, einsteinID, "policy_changed")
	if len(ev) != 2 || ev[1].Detail != "proactive_enabled" {
		t.Fatalf("events = %+v", ev)
	}
	updated := s.policy(t, einsteinID).UpdatedAt
	s.fake.Advance(time.Minute)
	mustOK(t, putSettings(s, `{"watch":["/SECRET-PATH"]}`))
	if n := len(s.auditKind(t, einsteinID, "policy_changed")); n != 2 {
		t.Fatalf("a no-op PUT wrote an event (%d)", n)
	}
	if !s.policy(t, einsteinID).UpdatedAt.Equal(updated) {
		t.Fatal("a no-op PUT rewrote the policy")
	}
	if err := s.st.View(bg, func(tx store.Tx) error { return tx.VerifyAudit(einsteinID) }); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSettingsPutsDoNotLoseUpdates(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	for i := range 10 {
		var wg sync.WaitGroup
		wg.Add(2)
		var r1, r2 *httptest.ResponseRecorder
		tokens, proactive := int64(1000+i), i%2 == 0
		go func() {
			defer wg.Done()
			r1 = putSettings(s, fmt.Sprintf(`{"daily_tokens":%d}`, tokens))
		}()
		go func() {
			defer wg.Done()
			r2 = putSettings(s, fmt.Sprintf(`{"proactive_enabled":%v,"quiet_hours":"0%d:00-17:00"}`, proactive, i))
		}()
		wg.Wait()
		if r1.Code != 200 || r2.Code != 200 {
			t.Fatalf("round %d: %d %d", i, r1.Code, r2.Code)
		}
		p := s.policy(t, einsteinID)
		if p.DailyTokens != tokens || p.ProactiveEnabled != proactive || p.QuietHours != fmt.Sprintf("0%d:00-17:00", i) {
			t.Fatalf("round %d lost an update: %+v", i, p)
		}
	}
}

func TestSettingsAreScopedToTheCaller(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	r := s.req(http.MethodPut, settingsPath+"?user_id="+marieID, `{"daily_tokens":2000}`)
	r.Header.Set("X-User", marieID)
	if rec := s.serve(r); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if s.policy(t, einsteinID).DailyTokens != 2000 || s.policy(t, marieID).DailyTokens == 2000 {
		t.Fatal("the PUT touched another user")
	}
	if got := settingsDoc(t, s.as("marie", http.MethodGet, settingsPath, "")); got.DailyTokens == 2000 {
		t.Fatal("marie sees einstein's settings")
	}
}

func TestSettingsNotEnrolledPut(t *testing.T) {
	s := newStack(t)
	wantError(t, putSettings(s, `{"daily_tokens":2000}`), http.StatusNotFound, "not_found")
}

// The handler validates by itself; the store's policy validation is a second
// line this test does not rely on.
func TestParseSettingsValidatesWithoutTheStore(t *testing.T) {
	s := &Server{d: Deps{Catalog: testCatalog, Cfg: Settings{UserDailyTokens: 200000}}}
	for _, tc := range []struct {
		name string
		body settingsBody
		ok   bool
	}{
		{"valid models", settingsBody{Models: []byte(`{"chat":"model-a","proactive":null}`)}, true},
		{"unknown task", settingsBody{Models: []byte(`{"summary":"model-a"}`)}, false},
		{"case-variant task", settingsBody{Models: []byte(`{"Chat":"model-a"}`)}, false},
		{"unknown task cleared", settingsBody{Models: []byte(`{"summary":null}`)}, false},
		{"fractional seconds", settingsBody{SweepInterval: []byte(`"5m0.5s"`)}, false},
		{"whole seconds", settingsBody{SweepInterval: []byte(`"5m1s"`)}, true},
		{"equal quiet ends", settingsBody{QuietHours: []byte(`"07:00-07:00"`)}, false},
		{"quiet hour 24", settingsBody{QuietHours: []byte(`"24:00-07:00"`)}, false},
		{"quiet ok", settingsBody{QuietHours: []byte(`"23:59-00:00"`)}, true},
		{"nothing", settingsBody{}, false},
		{"empty models", settingsBody{Models: []byte(`{}`)}, false},
	} {
		if _, err := s.parseSettings(tc.body); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// {"daily_tokens": null} goes back to following the operator's cap: the
// store keeps 0 and the settings show the cap.
func TestDailyTokensNullFollowsTheOperatorCap(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.UserDailyTokens = 300000 })
	s.enroll(t, "einstein")
	stored := func() int64 {
		var pol store.Policy
		s.mustTx(t, func(tx store.Tx) error {
			var err error
			pol, err = tx.GetPolicy(einsteinID)
			return err
		})
		return pol.DailyTokens
	}
	if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":5000}`); rec.Code != 200 || stored() != 5000 {
		t.Fatalf("lower: %d, stored %d", rec.Code, stored())
	}
	rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":null}`)
	if rec.Code != 200 {
		t.Fatalf("null: %d %s", rec.Code, rec.Body.String())
	}
	if m := decode(t, rec); m["daily_tokens"] != float64(300000) || stored() != 0 {
		t.Errorf("after null: %v, stored %d", m["daily_tokens"], stored())
	}
	evs := s.auditOf(t, einsteinID)
	if last := evs[len(evs)-1]; last.Kind != "policy_changed" || last.Detail != "daily_tokens" {
		t.Errorf("audit = %+v", last)
	}
	// null again changes nothing (no new audit row)
	n := len(s.auditOf(t, einsteinID))
	s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":null}`)
	if len(s.auditOf(t, einsteinID)) != n {
		t.Error("a no-op null was audited")
	}
}

// PUT /settings has its own 128 KiB body limit: a client that escapes
// every non-ASCII character reaches ~62 KiB with valid settings.
func TestSettingsBodiesUpTo128KiB(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	var watch []string
	for i := range maxWatchEntries {
		// 1 + 2 + 510 two-byte characters = 1023 bytes, each escaped as \u00e9
		watch = append(watch, fmt.Sprintf(`"/%02d%s"`, i, strings.Repeat(`\u00e9`, 510)))
	}
	body := `{"watch":[` + strings.Join(watch, ",") + `]}`
	pad := strings.Repeat(" ", 120<<10-len(body)) // whitespace is legal JSON
	if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", body+pad); rec.Code != 200 {
		t.Fatalf("an escaped body of %d bytes: %d %s", len(body+pad), rec.Code, rec.Body.String())
	}
	big := body + strings.Repeat(" ", 129<<10-len(body))
	wantError(t, s.as("einstein", http.MethodPut, "/agent-api/v1/settings", big), 413, "too_large")
}
