package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// ProactiveConfig holds the settings of the scheduled proactive sweep.
type ProactiveConfig struct {
	// Limits are the operator's caps; DailyJobs 0 means the default 24.
	Limits store.ProactiveLimits
	// PerTick caps the jobs one run starts (default 50), so a burst after
	// downtime is spread over several minutes.
	PerTick int
}

// skipAuditEvery throttles proactive_skipped: one event per user and reason
// within this time.
const skipAuditEvery = time.Hour

// maxProactivePages bounds the pages of due users one run reads.
const maxProactivePages = 10

// proactiveState is the in-memory state of the proactive task: the last user
// id visited (the rotation) and when a skip was last audited per user and
// reason. Losing it (a restart) costs at most one extra skip event per user
// and reason; the sweep marks themselves live in the database.
type proactiveState struct {
	after   string
	skipped map[skipKey]time.Time
}

type skipKey struct{ user, reason string }

func newProactiveState() proactiveState {
	return proactiveState{skipped: map[skipKey]time.Time{}}
}

// proactive action of one due user
type proAction int

const (
	proNone     proAction = iota // not due (any more) or not ours to report
	proBaseline                  // enabled without a last sweep: start counting now
	proStart                     // start a scheduled job
	proSkip                      // due but refused for a reason that is audited
)

// proactive starts the scheduled sweeps that are due. It reads only the
// database: the users whose sweep is due (ListProactiveDue) in user id
// order from the rotation cursor, at most maxProactivePages pages of Batch.
// For each it decides in a read transaction and writes only when there is
// something to write: a user seen enabled without a last sweep gets the
// baseline now (the first sweep comes one interval later); an eligible user
// gets a job (store.StartProactive, re-checked in the same transaction, so a
// crash or a second scheduler never starts two); a refused one gets a
// proactive_skipped event at most once per reason and hour, and keeps its
// last sweep, so it is retried at the next run. Nothing happens while the
// kill switch is on. A run ends after PerTick started jobs (the next run
// continues after the last user), at the end of the list (the rotation
// wraps) or after the page cap. Store errors of a user are counted and do
// not stop the run.
func (s *Scheduler) proactive(ctx context.Context, now time.Time) (int, int, error) {
	st := &s.pro
	for k, at := range st.skipped {
		if now.Sub(at) >= skipAuditEvery {
			delete(st.skipped, k)
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
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return started, len(errs), err
			}
			ok, err := s.proactiveUser(ctx, id, now)
			st.after = id
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ok {
				started++
				if started >= s.c.Proactive.PerTick {
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
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		act, reason, err = s.decideProactive(tx, userID, now)
		return err
	}); err != nil {
		return false, err
	}
	if act == proNone {
		return false, nil
	}
	if _, done := s.pro.skipped[skipKey{userID, reason}]; act == proSkip && done {
		return false, nil
	}
	errDone := errors.New("nothing to do")
	err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		// decide again: the state may have changed since the read
		var err error
		act, reason, err = s.decideProactive(tx, userID, now)
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
			if _, done := s.pro.skipped[skipKey{userID, reason}]; done {
				return errDone
			}
			_, err := tx.AppendAudit(audit.Event{
				UserID: userID, TS: now, Kind: audit.KindProactiveSkipped, Outcome: "skipped",
				Detail: "trigger=" + store.TriggerSchedule + " reason=" + reason,
			})
			return err
		}
		return errDone
	})
	switch {
	case errors.Is(err, errDone):
		return false, nil
	case err != nil:
		return false, err
	}
	if act == proSkip {
		s.pro.skipped[skipKey{userID, reason}] = now
	}
	return act == proStart, nil
}

// decideProactive decides what to do for a user ListProactiveDue returned.
// The kill switch, an unknown user or a disabled policy is proNone (the
// kill switch stops the whole run; the others are no longer due).
func (s *Scheduler) decideProactive(tx store.Tx, userID string, now time.Time) (proAction, string, error) {
	pol, err := tx.GetPolicy(userID)
	if errors.Is(err, store.ErrNotFound) {
		return proNone, "", nil
	}
	if err != nil {
		return proNone, "", err
	}
	if !pol.ProactiveEnabled {
		return proNone, "", nil
	}
	c, err := tx.GetCursor(userID)
	if err != nil {
		return proNone, "", err
	}
	if c.LastSweepAt == nil {
		return proBaseline, "", nil
	}
	if now.Before(c.LastSweepAt.Add(pol.SweepInterval)) {
		return proNone, "", nil
	}
	reason, err := store.CheckProactive(tx, userID, now, s.c.Proactive.Limits, true)
	if err != nil {
		return proNone, "", err
	}
	switch reason {
	case "":
		return proStart, "", nil
	case store.SkipNotEnrolled, store.SkipDisabled, store.SkipKillSwitch:
		return proNone, "", nil
	}
	return proSkip, reason, nil
}
