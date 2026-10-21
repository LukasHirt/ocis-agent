package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const proactiveRunPath = "/agent-api/v1/proactive/run"

// newProactiveStack has einstein and marie enrolled, both with proactive
// work enabled, a daily cap of 3 proactive jobs and a budget of 1000 tokens.
func newProactiveStack(t *testing.T) *stack {
	t.Helper()
	s := newStack(t, func(d *Deps) {
		d.Cfg.Proactive = store.ProactiveLimits{DailyJobs: 3}
		d.Cfg.UserDailyTokens = 1000
	})
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	for _, id := range []string{einsteinID, marieID} {
		s.setPolicy(t, id, func(p *store.Policy) { p.ProactiveEnabled = true })
	}
	return s
}

func (s *stack) runProactive(t *testing.T, login string) jobView {
	t.Helper()
	rec := s.as(login, http.MethodPost, proactiveRunPath, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d %s", rec.Code, rec.Body.String())
	}
	var v jobView
	strictDecode(t, rec.Body.String(), &v)
	return v
}

func (s *stack) endJobs(t *testing.T, user string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		jobs, err := tx.ListJobs(user, store.JobFilter{States: liveJobStates})
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if err := tx.CancelJob(user, j.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestProactiveRunStartsAManualReview(t *testing.T) {
	s := newProactiveStack(t)
	// quiet hours do not stop a review the user asked for
	now := s.fake.Now().UTC()
	s.setPolicy(t, einsteinID, func(p *store.Policy) {
		p.QuietHours = now.Format("15:04") + "-" + now.Add(time.Hour).Format("15:04")
	})
	wakes := s.wk.n
	v := s.runProactive(t, "einstein")
	goal, _ := store.ProactiveGoal(store.TriggerManual)
	if v.ID == "" || v.TriggerKind != store.TriggerManual || v.TriggerRef != "" || v.Goal != goal ||
		v.State != string(store.JobQueued) {
		t.Fatalf("job = %+v", v)
	}
	j := s.getStoredJob(t, einsteinID, v.ID)
	if j.Goal != goal || j.TriggerKind != store.TriggerManual || j.TriggerRef != "" {
		t.Errorf("stored job = %+v", j)
	}
	if s.wk.n != wakes+1 {
		t.Errorf("wakes = %d, want %d", s.wk.n, wakes+1)
	}
	evs := s.auditKind(t, einsteinID, audit.KindProactiveStarted)
	if len(evs) != 1 || evs[0].JobID != v.ID || evs[0].Cred != "oidc" || evs[0].Detail != "trigger=manual" {
		t.Errorf("audit = %+v", evs)
	}
	// the run counts as the user's last sweep
	var c store.Cursor
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		c, err = tx.GetCursor(einsteinID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c.LastSweepAt == nil || !c.LastSweepAt.Equal(s.fake.Now()) {
		t.Errorf("last sweep = %v", c.LastSweepAt)
	}
	// marie is not affected
	if jobs := s.listJobs(t, "marie", ""); len(jobs.Jobs) != 0 {
		t.Errorf("marie's jobs = %+v", jobs.Jobs)
	}
}

func TestProactiveRunRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s *stack)
		code  string
	}{
		{"disabled", func(t *testing.T, s *stack) {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.ProactiveEnabled = false })
		}, "proactive_disabled"},
		{"paused", func(t *testing.T, s *stack) {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseUser })
		}, "paused"},
		{"credentials revoked", func(t *testing.T, s *stack) {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseCredentialRevoked })
		}, "paused"},
		{"kill switch", func(t *testing.T, s *stack) {
			s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) })
		}, "kill_switch"},
		{"live job", func(t *testing.T, s *stack) {
			s.queuedJob(t, einsteinID, store.TriggerSchedule, "g")
		}, "live_job"},
		{"daily cap", func(t *testing.T, s *stack) {
			s.seedJobs(t, einsteinID, store.TriggerSchedule, 2)
			s.seedJobs(t, einsteinID, store.TriggerFileEvent, 1)
			s.seedJobs(t, einsteinID, store.TriggerChat, 5) // chat jobs do not count
		}, "daily_cap"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newProactiveStack(t)
			c.setup(t, s)
			wakes := s.wk.n
			rec := s.as("einstein", http.MethodPost, proactiveRunPath, "{}")
			wantError(t, rec, http.StatusConflict, c.code)
			if s.wk.n != wakes {
				t.Error("a refusal woke the queue")
			}
			if evs := s.auditKind(t, einsteinID, audit.KindProactiveStarted); len(evs) != 0 {
				t.Errorf("audit = %+v", evs)
			}
			// marie's own state decides for her
			s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, "0") })
			s.runProactive(t, "marie")
		})
	}
}

func TestProactiveRunBudget(t *testing.T) {
	for _, c := range []struct {
		used int64
		ok   bool
	}{{790, true}, {800, false}, {810, false}} {
		s := newProactiveStack(t)
		cl := s.startJob(t, einsteinID, store.TriggerChat, "hi")
		s.addStep(t, cl, store.JobStep{JobID: cl.Job.ID, Kind: "llm", Status: "ok", TokensIn: c.used})
		s.finishJob(t, cl, store.JobSucceeded, nil, "")
		rec := s.as("einstein", http.MethodPost, proactiveRunPath, "")
		if c.ok {
			if rec.Code != http.StatusAccepted {
				t.Errorf("used %d of 1000: %d %s", c.used, rec.Code, rec.Body.String())
			}
			continue
		}
		wantError(t, rec, http.StatusConflict, "budget")
	}
}

func TestProactiveRunEnvKillSwitch(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.KillSwitchEnv = true })
	s.enroll(t, "einstein")
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.ProactiveEnabled = true })
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, ""), http.StatusConflict, "kill_switch")
}

func TestProactiveRunNotEnrolledAndBadBodies(t *testing.T) {
	s := newProactiveStack(t)
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, `{"goal":"delete everything"}`), http.StatusBadRequest, "invalid_request")
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, `[]`), http.StatusBadRequest, "invalid_request")
	if rec := s.as("einstein", http.MethodGet, proactiveRunPath, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", rec.Code)
	}
	s.mustTx(t, func(tx store.Tx) error { return tx.DeleteUser(marieID) })
	wantError(t, s.as("marie", http.MethodPost, proactiveRunPath, ""), http.StatusConflict, "not_enrolled")
	if jobs := s.listJobs(t, "einstein", ""); len(jobs.Jobs) != 0 {
		t.Errorf("jobs = %+v", jobs.Jobs)
	}
}

// The run shares the 5 s cooldown of the oCIS-bound actions; a refused
// run uses it up too.
func TestProactiveRunSharesTheActionCooldown(t *testing.T) {
	s := newProactiveStack(t)
	s.srv.actions.every = actionCooldown
	s.runProactive(t, "einstein")
	rec := s.as("einstein", http.MethodPost, proactiveRunPath, "")
	wantError(t, rec, http.StatusTooManyRequests, "rate_limited")
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q", ra)
	}
	wantError(t, s.as("einstein", http.MethodPost, "/agent-api/v1/token/renew", ""), http.StatusTooManyRequests, "rate_limited")
	s.runProactive(t, "marie") // her own cooldown

	s.fake.Advance(actionCooldown)
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, ""), http.StatusConflict, "live_job")
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, ""), http.StatusTooManyRequests, "rate_limited")
	s.fake.Advance(actionCooldown)
	if rec := s.as("einstein", http.MethodPost, "/agent-api/v1/token/renew", ""); rec.Code != http.StatusOK {
		t.Fatalf("renew = %d %s", rec.Code, rec.Body.String())
	}
	s.endJobs(t, einsteinID)
	wantError(t, s.as("einstein", http.MethodPost, proactiveRunPath, ""), http.StatusTooManyRequests, "rate_limited")
	s.fake.Advance(actionCooldown)
	s.runProactive(t, "einstein")
}

// Enabling proactive work sets the sweep baseline, so the first scheduled
// sweep comes one interval later (also after a disable and enable).
func TestEnablingProactiveSetsTheSweepBaseline(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	cursor := func() store.Cursor {
		var c store.Cursor
		if err := s.st.View(bg, func(tx store.Tx) error {
			var err error
			c, err = tx.GetCursor(einsteinID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(body string) {
		t.Helper()
		if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", body); rec.Code != http.StatusOK {
			t.Fatalf("put %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	put(`{"quiet_hours":"22:00-07:00"}`)
	if c := cursor(); c.LastSweepAt != nil {
		t.Fatalf("baseline without enabling: %v", c.LastSweepAt)
	}
	put(`{"proactive_enabled":true}`)
	first := cursor().LastSweepAt
	if first == nil || !first.Equal(s.fake.Now()) {
		t.Fatalf("baseline = %v, want now", first)
	}
	s.fake.Advance(time.Hour)
	put(`{"proactive_enabled":true,"sweep_interval":"2h"}`) // already on: unchanged
	if got := cursor().LastSweepAt; !got.Equal(*first) {
		t.Errorf("baseline moved without an enable: %v", got)
	}
	put(`{"proactive_enabled":false}`)
	s.fake.Advance(time.Hour)
	put(`{"proactive_enabled":true}`)
	if got := cursor().LastSweepAt; got == nil || !got.Equal(s.fake.Now()) {
		t.Errorf("re-enabling: baseline = %v, want now", got)
	}
}
