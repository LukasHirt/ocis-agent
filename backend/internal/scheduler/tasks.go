package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// category maps an error to a fixed category. Only the category is ever
// logged, never the error text.
func category(err error) string {
	var status *ocisclient.StatusError
	var netErr net.Error
	switch {
	case errors.Is(err, ocisclient.ErrUnauthorized), errors.Is(err, ocisclient.ErrForbidden), errors.Is(err, auth.ErrCredentialInvalid):
		return CategoryUnauthorized
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr):
		return CategoryUnavailable
	case errors.As(err, &status) && status.Status >= 500:
		return CategoryUnavailable
	default:
		return CategoryInternal
	}
}

// itemLog logs per-item errors as counts by category.
func (s *Scheduler) itemLog(ctx context.Context, name string, errs []error) {
	if len(errs) == 0 {
		return
	}
	by := map[string]int{}
	for _, err := range errs {
		by[category(err)]++
	}
	attrs := []any{"task", name, "errors", len(errs)}
	for _, cat := range []string{CategoryUnauthorized, CategoryUnavailable, CategoryInternal} {
		if by[cat] > 0 {
			attrs = append(attrs, cat, by[cat])
		}
	}
	s.d.Log.Log(ctx, slog.LevelWarn, "scheduler: item errors", attrs...)
}

func (s *Scheduler) reap(ctx context.Context, _ time.Time) (int, int, error) {
	r, err := s.d.Queue.Reap(ctx, s.c.MaxAttempts)
	if err != nil {
		return 0, 0, err
	}
	return r.Requeued + r.Failed, 0, nil
}

func (s *Scheduler) expire(ctx context.Context, _ time.Time) (int, int, error) {
	n, err := s.d.Gate.ExpireAndResume(ctx)
	return n, 0, err
}

func (s *Scheduler) settle(ctx context.Context, _ time.Time) (int, int, error) {
	n, err := s.d.Gate.SettleEnded(ctx, s.c.Batch)
	return n, 0, err
}

// reconcile resolves the open calls of ended jobs that started before
// now - CallTimeout - RetrySettle (no call can still be in flight). An
// unauthorized error cannot be attributed to a user here; the owning token is
// handled by the next renewal or sweep, so only its category is logged. The
// executor stops when the run's deadline ends (its circuit break).
func (s *Scheduler) reconcile(ctx context.Context, now time.Time) (int, int, error) {
	cut := now.Add(-s.c.CallTimeout - s.c.RetrySettle)
	sum, err := s.d.Executor.ReconcileOrphans(ctx, s.d.Env, cut, s.c.Batch)
	s.itemLog(ctx, TaskReconcile, sum.Errors)
	return sum.Resolved, len(sum.Errors), err
}

// renew renews the due users one by one and stops at the first one that
// finds oCIS unavailable (or the run's deadline): the others wait for the
// next run instead of each waiting for their own timeout.
func (s *Scheduler) renew(ctx context.Context, _ time.Time) (int, int, error) {
	ids, err := s.d.Auth.DueForRenewal(ctx, s.c.Batch)
	if err != nil {
		return 0, 0, err
	}
	renewed := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if _, err := s.d.Auth.Renew(ctx, id); err != nil {
			errs = append(errs, err)
			if unavailable(ctx, err) {
				break
			}
			continue
		}
		renewed++
	}
	s.itemLog(ctx, TaskRenew, errs)
	return renewed, len(errs), nil
}

// unavailable reports an item error that means oCIS (or the run's time) is
// gone, so the rest of the batch would fail the same way.
func unavailable(ctx context.Context, err error) bool {
	return ctx.Err() != nil || category(err) == CategoryUnavailable
}

func (s *Scheduler) dormant(ctx context.Context, _ time.Time) (int, int, error) {
	n, err := s.d.Auth.PauseDormant(ctx, s.c.Batch)
	return n, 0, err
}

// maxSweepPages bounds the user pages one sweep run reads.
const maxSweepPages = 10

// sweepState is the in-memory cursor of the token sweep: the last user id
// visited, when each user was last swept, and which users the current pass
// has seen (to forget users that are gone when it wraps).
type sweepState struct {
	after string
	last  map[string]time.Time
	seen  map[string]bool
}

func newSweepState() sweepState {
	return sweepState{last: map[string]time.Time{}, seen: map[string]bool{}}
}

// sweepRun visits users in id order from the cursor and sweeps each one
// whose last sweep is at least SweepInterval ago; the others are skipped
// without a call. A run ends after Batch sweep calls, at the end of the user
// list (the cursor wraps), after maxSweepPages pages, or at the first user
// that finds oCIS unavailable or is cut off by the run's deadline: that user
// is not counted as swept and the next run starts with it. A user without a
// valid credential is skipped (enrolment or the renewal deals with it). The
// sweep time is recorded before the call, so a user that fails otherwise is
// retried after the interval, not on every run.
func (s *Scheduler) sweepRun(ctx context.Context, now time.Time) (int, int, error) {
	st := &s.sweep
	calls, swept, skipped, noCred := 0, 0, 0, 0
	var errs []error
	defer func() {
		s.itemLog(ctx, TaskSweep, errs)
		if calls > 0 {
			s.d.Log.Debug("scheduler: sweep run", "task", TaskSweep, "swept", swept, "skipped", skipped, "no_credential", noCred)
		}
	}()
	for page := 0; page < maxSweepPages; page++ {
		var users []store.User
		if err := s.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			users, err = tx.ListUsers(st.after, s.c.Batch)
			return err
		}); err != nil {
			return swept, len(errs), fmt.Errorf("scheduler: list users: %w", err)
		}
		for _, u := range users {
			if err := ctx.Err(); err != nil {
				return swept, len(errs), err
			}
			st.seen[u.ID] = true
			prev, had := st.last[u.ID]
			if had && now.Sub(prev) < s.c.SweepInterval {
				skipped++
				st.after = u.ID
				continue
			}
			st.last[u.ID] = now
			calls++
			res, err := s.d.Auth.Sweep(ctx, u.ID)
			switch {
			case err != nil && unavailable(ctx, err):
				// not swept: forget this attempt and resume at this user
				if had {
					st.last[u.ID] = prev
				} else {
					delete(st.last, u.ID)
				}
				errs = append(errs, err)
				return swept, len(errs), nil
			case errors.Is(err, auth.ErrCredentialInvalid):
				noCred++
			case err != nil:
				errs = append(errs, err)
			default:
				swept++
				if res.Unknown > 0 {
					s.d.Log.Warn("scheduler: token anomaly", "task", TaskSweep, "user", u.ID, "unknown", res.Unknown)
				}
			}
			st.after = u.ID
			if calls >= s.c.Batch {
				return swept, len(errs), nil
			}
		}
		if len(users) < s.c.Batch {
			s.wrapSweep()
			return swept, len(errs), nil
		}
	}
	return swept, len(errs), nil
}

// wrapSweep ends a pass: the cursor restarts at the first user and users the
// pass did not see (deleted) are forgotten.
func (s *Scheduler) wrapSweep() {
	st := &s.sweep
	for id := range st.last {
		if !st.seen[id] {
			delete(st.last, id)
		}
	}
	st.after = ""
	st.seen = map[string]bool{}
}
