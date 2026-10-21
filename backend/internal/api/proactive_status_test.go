package api

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func proactiveStatus(t *testing.T, s *stack, login string) proactiveView {
	t.Helper()
	return statusDoc(t, s.as(login, http.MethodGet, "/agent-api/v1/status", "")).Proactive
}

func at(t time.Time) *string { return timeView(t) }

// The status says whether proactive work could start now, when the last
// scheduled (or requested) review was and when the next one is expected.
func TestStatusShowsTheProactiveState(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.Proactive.DailyJobs = 7 })
	// not enrolled: the operator's limit, zero values otherwise, every key present
	rec := s.as("einstein", http.MethodGet, "/agent-api/v1/status", "")
	if got := statusDoc(t, rec).Proactive; !reflect.DeepEqual(got, proactiveView{DailyJobs: 7}) {
		t.Errorf("not enrolled = %+v", got)
	}
	m := decode(t, rec)["proactive"].(map[string]any)
	for _, k := range []string{"enabled", "last_sweep_at", "next_sweep_at", "blocked", "daily_jobs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing in %v", k, m)
		}
	}

	s.enroll(t, "einstein")
	if got, want := proactiveStatus(t, s, "einstein"), (proactiveView{Blocked: "proactive_disabled", DailyJobs: 7}); !reflect.DeepEqual(got, want) {
		t.Errorf("disabled = %+v, want %+v", got, want)
	}

	// switched on: the first review one interval (6h) later
	start := s.fake.Now() // 12:00 UTC
	mustOK(t, putSettings(s, `{"proactive_enabled":true}`))
	want := proactiveView{Enabled: true, LastSweepAt: at(start), NextSweepAt: at(start.Add(6 * time.Hour)), DailyJobs: 7}
	if got := proactiveStatus(t, s, "einstein"); !reflect.DeepEqual(got, want) {
		t.Errorf("enabled = %+v, want %+v", got, want)
	}

	// a next review inside the quiet hours moves to their end (UTC)
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.QuietHours = "17:00-19:30" })
	want.NextSweepAt = at(start.Add(7*time.Hour + 30*time.Minute))
	if got := proactiveStatus(t, s, "einstein"); !reflect.DeepEqual(got, want) {
		t.Errorf("quiet next = %+v, want %+v", got, want)
	}

	// quiet now
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.QuietHours = "11:00-13:00" })
	if got := proactiveStatus(t, s, "einstein"); got.Blocked != "quiet_hours" {
		t.Errorf("quiet now = %+v", got)
	}
	s.setPolicy(t, einsteinID, func(p *store.Policy) { p.QuietHours = "" })

	// overdue: due now
	s.fake.Advance(7 * time.Hour)
	if got := proactiveStatus(t, s, "einstein"); !reflect.DeepEqual(got.NextSweepAt, at(s.fake.Now())) || got.Blocked != "" {
		t.Errorf("overdue = %+v", got)
	}

	cases := []struct {
		name  string
		setup func()
		undo  func()
		code  string
	}{
		{"paused", func() {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseUser })
		}, func() {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = false, "" })
		}, "paused"},
		{"credential pause", func() {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseCredentialRevoked })
		}, func() {
			s.setPolicy(t, einsteinID, func(p *store.Policy) { p.Paused, p.PauseReason = false, "" })
		}, "credentials_invalid"},
		{"kill switch", func() {
			s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) })
		}, func() {
			s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, "0") })
		}, "kill_switch"},
	}
	for _, c := range cases {
		c.setup()
		if got := proactiveStatus(t, s, "einstein"); got.Blocked != c.code || !got.Enabled {
			t.Errorf("%s = %+v, want blocked %q", c.name, got, c.code)
		}
		c.undo()
	}

	// a live review
	s.queuedJob(t, einsteinID, store.TriggerManual, "g")
	if got := proactiveStatus(t, s, "einstein"); got.Blocked != "live_job" {
		t.Errorf("live job = %+v", got)
	}

	// switched off: no next review, the last one stays
	mustOK(t, putSettings(s, `{"proactive_enabled":false}`))
	want = proactiveView{LastSweepAt: at(start), Blocked: "proactive_disabled", DailyJobs: 7}
	if got := proactiveStatus(t, s, "einstein"); !reflect.DeepEqual(got, want) {
		t.Errorf("off = %+v, want %+v", got, want)
	}
}

// Every code the status can report in proactive.blocked is a documented
// refusal code of POST /proactive/run or quiet_hours.
func TestProactiveBlockedCodes(t *testing.T) {
	for reason := range proactiveRefusals {
		if reason == store.SkipNotEnrolled {
			continue
		}
		if proactiveBlocked(reason, "") == "" {
			t.Errorf("reason %q has no code", reason)
		}
	}
	if got := proactiveBlocked(store.SkipQuietHours, ""); got != "quiet_hours" {
		t.Errorf("quiet hours = %q", got)
	}
	if got := proactiveBlocked(store.SkipPaused, store.PauseCredentialExpired); got != "credentials_invalid" {
		t.Errorf("credential pause = %q", got)
	}
	if got := proactiveBlocked(store.SkipNotEnrolled, ""); got != "" {
		t.Errorf("not enrolled = %q", got)
	}
}
