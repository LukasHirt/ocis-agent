package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// A user who has not enrolled sees the global state (both kill switch
// flags, the operator's models) and zero values for everything of their own.
func TestStatusOfAnUnknownUserIsNotEnrolled(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.KillSwitchEnv = true })
	s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) })
	rec := s.do(http.MethodGet, "/agent-api/v1/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	got := statusDoc(t, rec)
	want := statusView{
		Token:      tokenView{Status: "none"},
		KillSwitch: killSwitchView{Database: true, Environment: true},
		Models: modelsView{
			Default:   "model-a",
			Available: []modelView{{ID: "model-a", Label: "Model A"}, {ID: "model-b", Label: "Model B"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("not enrolled = %+v, want %+v", got, want)
	}
	// every key is present (zero values, not missing fields)
	m := decode(t, rec)
	for _, k := range []string{"enrolled", "user", "paused", "pause_reason", "token", "budget", "kill_switch", "models", "pending_approvals"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing in %s", k, rec.Body.String())
		}
	}
	if _, found := s.user(t, einsteinID); found {
		t.Fatal("a status request must not create the user")
	}
}

func TestStatusOfAnEnrolledUser(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	rec := s.do(http.MethodGet, "/agent-api/v1/status", "")
	got := statusDoc(t, rec)
	tok := s.appToken(t, einsteinID)
	exp := tok.ExpiresAt.UTC().Format(time.RFC3339)
	resets := "2026-10-15T00:00:00Z" // the fake's clock starts 2026-10-14 12:00 UTC
	want := statusView{
		Enrolled: true,
		User:     userView{ID: einsteinID, Name: "Einstein", Login: "einstein"},
		Token:    tokenView{Status: "valid", ExpiresAt: &exp},
		Budget:   budgetView{UsedTokens: 0, DailyTokens: 200000, ResetsAt: &resets},
		Models: modelsView{
			Default:   "model-a",
			Available: []modelView{{ID: "model-a", Label: "Model A"}, {ID: "model-b", Label: "Model B"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status =\n%+v\nwant\n%+v", got, want)
	}
	body := rec.Body.String()
	for _, bad := range []string{"apptoken-", "hash-of", "ocis-agent-", tok.Label, "oidc."} {
		if strings.Contains(body, bad) {
			t.Errorf("status leaks %q: %s", bad, body)
		}
	}
	// preferences are null, not "" (no choice made)
	m := decode(t, rec)
	prefs := m["models"].(map[string]any)["preferences"].(map[string]any)
	if v, ok := prefs["chat"]; !ok || v != nil {
		t.Errorf("chat preference = %v (present %v), want null", v, ok)
	}
}

func TestStatusIsScopedToTheCaller(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.PausePolicy(marieID, store.PauseUser)
		return err
	})
	for _, target := range []string{"/agent-api/v1/status", "/agent-api/v1/status?user_id=" + marieID} {
		r := s.req(http.MethodGet, target, "")
		r.Header.Set("X-User", marieID)
		got := statusDoc(t, s.serve(r))
		if got.User.ID != einsteinID || got.Paused {
			t.Errorf("%s: %+v", target, got)
		}
	}
	got := statusDoc(t, s.as("marie", http.MethodGet, "/agent-api/v1/status", ""))
	if got.User.ID != marieID || !got.Paused || got.PauseReason != store.PauseUser {
		t.Errorf("marie = %+v", got)
	}
}

// appendStep runs a job of the user and records one step with these token
// counts at the current (fake) time.
func (s *stack) appendStep(t *testing.T, userID string, in, out int64) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.InsertJob(store.Job{UserID: userID, TriggerKind: store.TriggerChat, Goal: "g"})
		return err
	})
	var c store.Claim
	s.mustTx(t, func(tx store.Tx) error {
		var ok bool
		var err error
		c, ok, err = tx.ClaimNextJob("w", time.Minute)
		if err == nil && !ok {
			t.Fatal("nothing to claim")
		}
		return err
	})
	s.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.AppendStep(store.JobStep{JobID: c.Job.ID, Kind: "llm", Status: "ok", TokensIn: in, TokensOut: out}, c.Fence); err != nil {
			return err
		}
		_, err := tx.ReleaseJob(c, store.JobSucceeded, nil, "")
		return err
	})
}

func TestStatusBudgetCountsTheCurrentUTCDay(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	// 2026-10-14 12:00 UTC
	s.appendStep(t, einsteinID, 1000, 0)
	s.appendStep(t, marieID, 99999, 0)                             // another user's usage never counts
	s.fake.Advance(11*time.Hour + 59*time.Minute + 59*time.Second) // 23:59:59
	s.appendStep(t, einsteinID, 100, 50)

	b := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget
	if b.UsedTokens != 1150 || b.ResetsAt == nil || *b.ResetsAt != "2026-10-15T00:00:00Z" {
		t.Fatalf("before midnight = %+v (resets %v)", b, deref(b.ResetsAt))
	}

	s.fake.Advance(time.Second) // exactly 2026-10-15 00:00:00
	b = statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget
	if b.UsedTokens != 0 || *b.ResetsAt != "2026-10-16T00:00:00Z" {
		t.Fatalf("at midnight = %+v (resets %v)", b, deref(b.ResetsAt))
	}
	s.appendStep(t, einsteinID, 7, 3) // a step at 00:00:00 belongs to the new day
	b = statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget
	if b.UsedTokens != 10 {
		t.Fatalf("after midnight = %+v", b)
	}
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestStatusBudgetNeverExceedsTheOperatorCap(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.UserDailyTokens = 50000 })
	s.enroll(t, "einstein")
	if b := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget; b.DailyTokens != 50000 {
		t.Fatalf("default policy (200000) above the cap = %d, want 50000", b.DailyTokens)
	}
	setDaily := func(n int64) {
		s.mustTx(t, func(tx store.Tx) error {
			p, err := tx.GetPolicy(einsteinID)
			if err != nil {
				return err
			}
			p.DailyTokens = n
			return tx.PutPolicy(p)
		})
	}
	setDaily(1000)
	if b := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget; b.DailyTokens != 1000 {
		t.Fatalf("a lower user budget = %d, want 1000", b.DailyTokens)
	}
	setDaily(0) // "unlimited" in the store is still capped by the operator
	if b := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Budget; b.DailyTokens != 50000 {
		t.Fatalf("an unlimited user budget = %d, want the cap", b.DailyTokens)
	}
}

func TestStatusKillSwitchFlagsAreSeparate(t *testing.T) {
	for _, c := range []struct {
		name    string
		db, env bool
	}{{"off", false, false}, {"database", true, false}, {"environment", false, true}, {"both", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			s := newStack(t, func(d *Deps) { d.Cfg.KillSwitchEnv = c.env })
			s.enroll(t, "einstein")
			if c.db {
				s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) })
			} else {
				// any other value is off, like agent.KillSwitchOn
				s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, "0") })
			}
			ks := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).KillSwitch
			if ks.Database != c.db || ks.Environment != c.env {
				t.Fatalf("kill_switch = %+v, want db %v env %v", ks, c.db, c.env)
			}
		})
	}
}

func TestStatusShowsOnlyPreferencesInTheCatalog(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.mustTx(t, func(tx store.Tx) error {
		p, err := tx.GetPolicy(einsteinID)
		if err != nil {
			return err
		}
		p.Models = map[string]string{store.ModelTaskChat: "model-b", store.ModelTaskProactive: "retired-model"}
		return tx.PutPolicy(p)
	})
	pr := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).Models.Preferences
	if pr.Chat == nil || *pr.Chat != "model-b" || pr.Proactive != nil {
		t.Fatalf("preferences = chat %v proactive %v", deref(pr.Chat), deref(pr.Proactive))
	}
}

// waitingJob claims the next job of the user, files approvals for it and
// releases it to waiting_approval, like the gate does.
func (s *stack) waitingJob(t *testing.T, userID string, approvals int) (store.Job, []store.Approval) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.InsertJob(store.Job{UserID: userID, TriggerKind: store.TriggerChat, Goal: "g"})
		return err
	})
	var out []store.Approval
	var job store.Job
	s.mustTx(t, func(tx store.Tx) error {
		c, ok, err := tx.ClaimNextJob("w", time.Minute)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("nothing to claim")
		}
		job = c.Job
		for i := 0; i < approvals; i++ {
			a, err := tx.InsertApproval(store.Approval{
				UserID: userID, JobID: c.Job.ID, CallID: "c" + string(rune('0'+i)), Tool: "files.delete",
				Risk: store.RiskDestructive, Args: json.RawMessage(`{"path":"/x"}`), ArgsHash: "sha256:abc",
				Preview: json.RawMessage(`{}`), IdemKey: c.Job.ID + ":c" + string(rune('0'+i)),
				ExpiresAt: tx.Now().Add(time.Hour),
			}, c.Fence)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		_, err = tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		return err
	})
	return job, out
}

func TestStatusPendingApprovalsCountsLiveJobsOnly(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	s.waitingJob(t, einsteinID, 2) // live: counts 2

	ended, _ := s.waitingJob(t, einsteinID, 1) // ended: its pending approval is not counted
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, ended.ID) })

	_, decided := s.waitingJob(t, einsteinID, 1) // settled: not pending any more
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.DecideApproval(einsteinID, decided[0].ID, store.ApprovalRejected, einsteinID, decided[0].ArgsHash)
		return err
	})
	s.waitingJob(t, marieID, 3) // another user's

	if n := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", "")).PendingApprovals; n != 2 {
		t.Fatalf("pending_approvals = %d, want 2", n)
	}
	if n := statusDoc(t, s.as("marie", http.MethodGet, "/agent-api/v1/status", "")).PendingApprovals; n != 3 {
		t.Fatalf("marie's pending_approvals = %d, want 3", n)
	}
}

func TestStatusShowsPauseAndTokenState(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.mustTx(t, func(tx store.Tx) error {
		if err := tx.SetAppTokenStatus(einsteinID, store.TokenInvalid); err != nil {
			return err
		}
		_, err := tx.PausePolicy(einsteinID, store.PauseCredentialInvalid)
		return err
	})
	got := statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", ""))
	if !got.Paused || got.PauseReason != store.PauseCredentialInvalid || got.Token.Status != "invalid" || got.Token.ExpiresAt == nil {
		t.Fatalf("invalid token = %+v", got)
	}
	s.mustTx(t, func(tx store.Tx) error { return tx.WipeAppToken(einsteinID) })
	got = statusDoc(t, s.do(http.MethodGet, "/agent-api/v1/status", ""))
	if got.Token.Status != "revoked" || got.Token.ExpiresAt != nil {
		t.Fatalf("revoked token = %+v", got.Token)
	}
}

// countingStore counts View and InTx calls.
type countingStore struct {
	store.Store
	views, txs int
}

func (c *countingStore) View(ctx context.Context, fn func(store.Tx) error) error {
	c.views++
	return c.Store.View(ctx, fn)
}

func (c *countingStore) InTx(ctx context.Context, fn func(store.Tx) error) error {
	c.txs++
	return c.Store.InTx(ctx, fn)
}

func TestStatusReadsInOneView(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	cs := &countingStore{Store: s.st}
	s.srv.d.Store = cs
	// Seen (throttled: already recorded by the enrolment request) is the
	// pipeline's write, not the handler's
	rec := s.do(http.MethodGet, "/agent-api/v1/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cs.views != 1 || cs.txs != 0 {
		t.Fatalf("status used %d views and %d transactions, want 1 and 0", cs.views, cs.txs)
	}
}

// A new user starts at the operator's cap (AGENT_USER_DAILY_TOKENS is the
// cap AND the default): the stored value is 0, which every reader resolves
// to the cap, so raising the cap raises it for users who never changed it.
func TestANewUserStartsAtTheOperatorsCap(t *testing.T) {
	s := newStack(t, func(d *Deps) { d.Cfg.UserDailyTokens = 1000000 })
	s.enroll(t, "einstein")
	if v := statusDoc(t, s.as("einstein", http.MethodGet, "/agent-api/v1/status", "")); v.Budget.DailyTokens != 1000000 {
		t.Errorf("status daily_tokens = %d", v.Budget.DailyTokens)
	}
	settings := decode(t, s.as("einstein", http.MethodGet, "/agent-api/v1/settings", ""))
	if settings["daily_tokens"] != float64(1000000) || settings["daily_tokens_max"] != float64(1000000) {
		t.Errorf("settings = %v", settings)
	}
	stored := func() int64 {
		var pol store.Policy
		s.mustTx(t, func(tx store.Tx) error {
			var err error
			pol, err = tx.GetPolicy(einsteinID)
			return err
		})
		return pol.DailyTokens
	}
	if n := stored(); n != 0 {
		t.Errorf("stored daily tokens = %d, want 0 (the operator's value)", n)
	}
	// changing another setting keeps it untouched
	if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"proactive_enabled":true}`); rec.Code != 200 {
		t.Fatalf("put = %d", rec.Code)
	}
	if n := stored(); n != 0 {
		t.Errorf("stored daily tokens after a PUT of another field = %d", n)
	}
	// the user can still lower it, but never store 0 explicitly
	wantError(t, s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":0}`), 400, "invalid_request")
	if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":5000}`); rec.Code != 200 {
		t.Fatalf("put = %d", rec.Code)
	}
	if n := stored(); n != 5000 {
		t.Errorf("stored daily tokens = %d", n)
	}
}
