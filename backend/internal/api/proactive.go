package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// defaultProactiveDailyJobs is the proactive jobs per user and UTC day when
// Settings.Proactive.DailyJobs is 0 (AGENT_PROACTIVE_DAILY_JOBS).
const defaultProactiveDailyJobs = 24

// errCredentialsPaused refuses a manual review while a credential_* pause
// is in place (the same code as auth.ErrCredentialInvalid).
var errCredentialsPaused = newAPIError(http.StatusConflict, "credentials_invalid")

// Refusals of a manual review, by store.CheckProactive reason.
var proactiveRefusals = map[string]*apiError{
	store.SkipNotEnrolled: errNotEnrolled,
	store.SkipDisabled:    newAPIError(http.StatusConflict, "proactive_disabled"),
	store.SkipKillSwitch:  newAPIError(http.StatusConflict, "kill_switch"),
	store.SkipPaused:      newAPIError(http.StatusConflict, "paused"),
	store.SkipLiveJob:     newAPIError(http.StatusConflict, "live_job"),
	store.SkipDailyCap:    newAPIError(http.StatusConflict, "daily_cap"),
	store.SkipBudget:      newAPIError(http.StatusConflict, "budget"),
}

// runProactive is POST /agent-api/v1/proactive/run with an empty body or {}:
// start a review now (trigger kind manual, the fixed goal). The checks of
// the scheduled sweep apply, in the same transaction as the insert, except
// the quiet hours and the sweep interval (the user asked for it): enrolled
// (409 not_enrolled), proactive work enabled, kill switch off, not paused
// (a credential_* pause is 409 credentials_invalid: enrol again),
// no live proactive job, the daily cap and 80% of the token budget (409
// with the reason's code). The run counts as the user's last sweep. It
// shares the cooldown of the oCIS-bound actions, taken before the checks,
// so a refused run uses it up too. 202 with the job; the queue is woken.
func (s *Server) runProactive(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	if !s.actionAllowed(w, r, p.UserID) {
		return nil
	}
	now := s.d.Now()
	var job store.Job
	err := s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		reason, err := store.CheckProactive(tx, p.UserID, now, s.d.Cfg.Proactive, false)
		if err != nil {
			return err
		}
		if reason == store.SkipPaused {
			// a credential pause ends only with a new enrolment: say so
			pol, err := tx.GetPolicy(p.UserID)
			if err != nil {
				return err
			}
			if strings.HasPrefix(pol.PauseReason, "credential_") {
				return errCredentialsPaused
			}
		}
		if reason != "" {
			if e, ok := proactiveRefusals[reason]; ok {
				return e
			}
			return errors.New("api: unexpected proactive refusal")
		}
		job, err = store.StartProactive(tx, p.UserID, store.TriggerManual, now, "oidc")
		return err
	})
	if err != nil {
		return err
	}
	s.d.Queue.Wake()
	writeJSON(w, http.StatusAccepted, jobViewOf(summaryOf(job, nil)))
	return nil
}
