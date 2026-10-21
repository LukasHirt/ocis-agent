package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// ProactiveConfig holds the settings of the scheduled proactive sweep.
type ProactiveConfig struct {
	// Limits are the operator's caps; DailyJobs 0 means the default 24.
	// DailyTokens (AGENT_USER_DAILY_TOKENS) is required: unlike the API,
	// the scheduler has no other cap to default to, and 0 would leave users
	// with a stored 0 without any budget.
	Limits store.ProactiveLimits
	// PerTick caps the jobs one run starts (default 50), so a burst after
	// downtime is spread over several minutes.
	PerTick int
}

// maxProactivePages bounds the pages of due users one run reads. Users who
// wait (proactiveState.notBefore) cost no query beyond their page.
const maxProactivePages = 50

// How long a skipped user waits before the next check. It only saves work:
// every check decides from the database again, so a condition that ends
// earlier delays the next sweep by at most the wait.
const (
	waitLiveJob = 2 * time.Minute  // a job usually ends in minutes; approvals may take days
	waitPaused  = 5 * time.Minute  // a resume is picked up within this
	waitBudget  = 15 * time.Minute // the user may raise daily_tokens; at most until UTC midnight
	waitQuiet   = 15 * time.Minute // a malformed quiet hours value (always quiet)
)

// proactiveState is the in-memory state of the proactive task: the last user
// id visited (the rotation) and, per skipped user, when to check again.
// Losing it (a restart) only costs one extra check per user; the sweep
// marks and the skip audit rule live in the database.
type proactiveState struct {
	after     string
	notBefore map[string]time.Time
}

func newProactiveState() proactiveState {
	return proactiveState{notBefore: map[string]time.Time{}}
}

// skipWait is when a user skipped for reason at now is checked again.
func skipWait(reason string, pol store.Policy, now time.Time) time.Time {
	midnight := store.StartOfUTCDay(now).AddDate(0, 0, 1)
	switch reason {
	case store.SkipDailyCap:
		return midnight
	case store.SkipBudget:
		return minTime(now.Add(waitBudget), midnight)
	case store.SkipQuietHours:
		if end := store.QuietHoursEnd(pol.QuietHours, now); !end.IsZero() {
			return end
		}
		return now.Add(waitQuiet)
	case store.SkipPaused:
		return now.Add(waitPaused)
	}
	return now.Add(waitLiveJob)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// proactive action of one due user
type proAction int

const (
	proNone     proAction = iota // not due (any more) or not ours to report
	proBaseline                  // enabled without a last sweep: start counting now
	proStart                     // start a scheduled job
	proSkip                      // due but refused (CheckProactive's reason)
)

// proactive starts the scheduled sweeps that are due. It reads only the
// database: the users whose sweep is due (ListProactiveDue) in user id
// order from the rotation cursor, at most maxProactivePages pages of Batch.
// A user still waiting after a skip (skipWait) is passed over without a
// query. For the others it decides in a read transaction and writes only
// when there is something to write: a user seen enabled without a last
// sweep gets the baseline now (the first sweep comes one interval later); an
// eligible user gets a job (store.StartProactive, re-checked in the same
// transaction, so a crash or a second scheduler never starts two); a refused
// one keeps its last sweep (it is retried after its wait) and the skip is
// audited by store.AuditProactiveSkip's rule (caps only, on a change of
// reason or once a day). Nothing happens while the kill switch is on. A run
// ends after PerTick started jobs (the next run continues after the last
// user; at the end of the list it wraps at once), at the end of the list
// (the rotation wraps) or after the page cap. Store errors of a user are
// counted and do not stop the run.
func (s *Scheduler) proactive(ctx context.Context, now time.Time) (int, int, error) {
	st := &s.pro
	for id, at := range st.notBefore {
		if !now.Before(at) {
			delete(st.notBefore, id)
		}
	}
	var kill bool
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		kill, err = store.KillSwitchOn(tx)
		return err
	}); err != nil {
		return 0, 0, fmt.Errorf("scheduler: kill switch: %w", err)
	}
	if kill || s.c.Proactive.Limits.KillSwitchEnv {
		return 0, 0, nil
	}
	started := 0
	var errs []error
	defer func() {
		if started > 0 {
			s.d.Queue.Wake()
		}
		s.itemLog(ctx, TaskProactive, errs)
	}()
	for page := 0; page < maxProactivePages; page++ {
		var ids []string
		if err := s.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			ids, err = tx.ListProactiveDue(now, st.after, s.c.Batch)
			return err
		}); err != nil {
			return started, len(errs), fmt.Errorf("scheduler: list due users: %w", err)
		}
		for i, id := range ids {
			if err := ctx.Err(); err != nil {
				return started, len(errs), err
			}
			st.after = id
			if at, ok := st.notBefore[id]; ok && now.Before(at) {
				continue
			}
			ok, err := s.proactiveUser(ctx, id, now)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ok {
				started++
				if started >= s.c.Proactive.PerTick {
					if i == len(ids)-1 && len(ids) < s.c.Batch {
						st.after = "" // the end of the list: wrap now
					}
					return started, len(errs), nil
				}
			}
		}
		if len(ids) < s.c.Batch {
			st.after = ""
			return started, len(errs), nil
		}
	}
	return started, len(errs), nil
}

// proactiveUser handles one due user and reports whether it started a job.
func (s *Scheduler) proactiveUser(ctx context.Context, userID string, now time.Time) (bool, error) {
	var act proAction
	var reason string
	var pol store.Policy
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		act, reason, pol, err = s.decideProactive(tx, userID, now)
		return err
	}); err != nil {
		return false, err
	}
	if act == proNone {
		return false, nil
	}
	if act == proSkip && !store.SkipAudited(reason) {
		// nothing to record (paused, quiet hours): no write transaction
		s.pro.notBefore[userID] = skipWait(reason, pol, now)
		return false, nil
	}
	err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		// decide again: the state may have changed since the read
		var err error
		act, reason, pol, err = s.decideProactive(tx, userID, now)
		if err != nil {
			return err
		}
		switch act {
		case proBaseline:
			c, err := tx.GetCursor(userID)
			if err != nil {
				return err
			}
			at := now
			c.LastSweepAt = &at
			return tx.PutCursor(c)
		case proStart:
			_, err := store.StartProactive(tx, userID, store.TriggerSchedule, now, "")
			return err
		case proSkip:
			_, err := store.AuditProactiveSkip(tx, userID, store.TriggerSchedule, reason, now)
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if act == proSkip {
		s.pro.notBefore[userID] = skipWait(reason, pol, now)
	}
	return act == proStart, nil
}

// decideProactive decides what to do for a user ListProactiveDue returned.
// The kill switch, an unknown user or a disabled policy is proNone (the
// kill switch stops the whole run; the others are no longer due).
func (s *Scheduler) decideProactive(tx store.Tx, userID string, now time.Time) (proAction, string, store.Policy, error) {
	pol, err := tx.GetPolicy(userID)
	if errors.Is(err, store.ErrNotFound) {
		return proNone, "", pol, nil
	}
	if err != nil {
		return proNone, "", pol, err
	}
	if !pol.ProactiveEnabled {
		return proNone, "", pol, nil
	}
	c, err := tx.GetCursor(userID)
	if err != nil {
		return proNone, "", pol, err
	}
	if c.LastSweepAt == nil {
		return proBaseline, "", pol, nil
	}
	if now.Before(c.LastSweepAt.Add(pol.SweepInterval)) {
		return proNone, "", pol, nil
	}
	reason, err := store.CheckProactive(tx, userID, now, s.c.Proactive.Limits, true)
	if err != nil {
		return proNone, "", pol, err
	}
	switch reason {
	case "":
		return proStart, "", pol, nil
	case store.SkipNotEnrolled, store.SkipDisabled, store.SkipKillSwitch:
		return proNone, "", pol, nil
	}
	return proSkip, reason, pol, nil
}
