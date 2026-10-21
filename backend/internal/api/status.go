package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// getStatus is GET /agent-api/v1/status.
func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	v, err := s.status(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

var errNoPrincipal = errors.New("api: no principal on an authenticated route")

// status computes the caller's status in ONE read transaction, so the parts
// agree with each other. Everything is scoped by the principal's user id.
func (s *Server) status(ctx context.Context, p Principal) (statusView, error) {
	now := s.d.Now()
	dayStart := startOfUTCDay(now)
	v := statusView{Token: tokenView{Status: "none"}, Models: modelsView{Available: []modelView{}}}
	err := s.d.Store.View(ctx, func(tx store.Tx) error {
		// the global state, also for a user who has not enrolled
		switch ks, err := tx.GetSystem(store.KillSwitchKey); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			v.KillSwitch.Database = ks == store.KillSwitchOnValue
		}
		v.KillSwitch.Environment = s.d.Cfg.KillSwitchEnv
		v.Models = s.modelsView(store.Policy{})
		// the budget a new enrolment starts with (never 0, which a client
		// could read as unlimited)
		v.Budget = budgetView{DailyTokens: s.dailyBudget(0), ResetsAt: timeView(dayStart.AddDate(0, 0, 1))}
		v.Proactive = proactiveView{DailyJobs: s.d.Cfg.Proactive.DailyJobs}

		u, err := tx.GetUser(p.UserID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // not enrolled: zero values for the user's own state
		}
		if err != nil {
			return err
		}
		pol, err := tx.GetPolicy(p.UserID)
		if err != nil {
			return err
		}
		v.Enrolled = true
		v.User = userView{ID: u.ID, Name: p.DisplayName, Login: u.Name}
		v.Paused, v.PauseReason = pol.Paused, pol.PauseReason

		switch tok, err := tx.GetAppToken(p.UserID); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			v.Token.Status = string(tok.Status)
			if tok.Status != store.TokenRevoked {
				v.Token.ExpiresAt = timeView(tok.ExpiresAt)
			}
		}

		in, out, err := tx.SumJobTokens(p.UserID, dayStart)
		if err != nil {
			return err
		}
		v.Budget = budgetView{
			UsedTokens:  in + out,
			DailyTokens: s.dailyBudget(pol.DailyTokens), // what the runner enforces
			ResetsAt:    timeView(dayStart.AddDate(0, 0, 1)),
		}

		v.Models = s.modelsView(pol)

		if v.Proactive, err = s.proactiveState(tx, p.UserID, pol, now); err != nil {
			return err
		}

		// the decidable ones: pending approvals of jobs that have not ended
		v.PendingApprovals, err = tx.CountPendingOfLiveJobs(p.UserID)
		return err
	})
	if err != nil {
		return statusView{}, err
	}
	return v, nil
}

// proactiveState is the proactiveView of the user at now, read in tx.
func (s *Server) proactiveState(tx store.Tx, userID string, pol store.Policy, now time.Time) (proactiveView, error) {
	reason, err := store.CheckProactive(tx, userID, now, s.d.Cfg.Proactive, true)
	if err != nil {
		return proactiveView{}, err
	}
	c, err := tx.GetCursor(userID)
	if err != nil {
		return proactiveView{}, err
	}
	v := proactiveView{
		Enabled: pol.ProactiveEnabled, Blocked: proactiveBlocked(reason, pol.PauseReason), DailyJobs: s.d.Cfg.Proactive.DailyJobs,
	}
	if c.LastSweepAt != nil {
		v.LastSweepAt = timeView(*c.LastSweepAt)
		if pol.ProactiveEnabled {
			next := c.LastSweepAt.Add(pol.SweepInterval)
			if next.Before(now) {
				next = now
			}
			if end := store.QuietHoursEnd(pol.QuietHours, next); !end.IsZero() {
				next = end
			}
			v.NextSweepAt = timeView(next)
		}
	}
	return v, nil
}

// proactiveBlocked is the code of a store.CheckProactive reason in the
// status: the refusal code of POST /proactive/run (credentials_invalid for a
// credential_* pause), quiet_hours, or "" (none, or not enrolled).
func proactiveBlocked(reason, pauseReason string) string {
	switch reason {
	case "", store.SkipNotEnrolled:
		return ""
	case store.SkipQuietHours:
		return "quiet_hours"
	case store.SkipPaused:
		if strings.HasPrefix(pauseReason, "credential_") {
			return errCredentialsPaused.code
		}
	}
	if e, ok := proactiveRefusals[reason]; ok {
		return e.code
	}
	return reason
}

// dailyBudget is the user's budget under the operator's cap
// (store.DailyBudget, the function the runner's budget check uses).
func (s *Server) dailyBudget(stored int64) int64 {
	return store.DailyBudget(stored, s.d.Cfg.UserDailyTokens)
}

func (s *Server) modelsView(pol store.Policy) modelsView {
	mv := modelsView{Default: s.d.Catalog.Default, Available: make([]modelView, 0, len(s.d.Catalog.Models))}
	for _, m := range s.d.Catalog.Models {
		mv.Available = append(mv.Available, modelView{ID: m.ID, Label: m.Label})
	}
	pref := func(task string) *string {
		id := pol.Models[task]
		if id == "" || !s.d.Catalog.Has(id) {
			return nil
		}
		return &id
	}
	mv.Preferences = preferencesView{Chat: pref(store.ModelTaskChat), Proactive: pref(store.ModelTaskProactive)}
	return mv
}

// startOfUTCDay is the UTC midnight at or before t (the budget day, as the
// runner counts it).
func startOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
