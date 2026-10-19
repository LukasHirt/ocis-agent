package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// maxPendingScan bounds the pending approvals status counts (the store's
// ListApprovals maximum); a count at this value means "at least".
const maxPendingScan = 500

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

		v.PendingApprovals, err = pendingOfLiveJobs(tx, p.UserID)
		return err
	})
	if err != nil {
		return statusView{}, err
	}
	return v, nil
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

// pendingOfLiveJobs counts the user's pending approvals whose job has not
// ended (queued, running or waiting_approval). Approvals of an ended job
// stay pending until the scheduler settles them; they cannot be decided.
func pendingOfLiveJobs(tx store.Tx, userID string) (int, error) {
	list, err := tx.ListApprovals(userID, store.ApprovalFilter{States: []store.ApprovalState{store.ApprovalPending}, Limit: maxPendingScan})
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	n := 0
	for _, a := range list {
		ok, seen := live[a.JobID]
		if !seen {
			j, err := tx.GetJobStatus(userID, a.JobID)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return 0, err
			default:
				ok = !j.State.Terminal()
			}
			live[a.JobID] = ok
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// startOfUTCDay is the UTC midnight at or before t (the budget day, as the
// runner counts it).
func startOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
