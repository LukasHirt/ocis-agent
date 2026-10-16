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
// handled by the next renewal or sweep, so only its category is logged.
func (s *Scheduler) reconcile(ctx context.Context, now time.Time) (int, int, error) {
	cut := now.Add(-s.c.CallTimeout - s.c.RetrySettle)
	sum, err := s.d.Executor.ReconcileOrphans(ctx, s.d.Env, cut, s.c.Batch)
	if err != nil {
		return 0, 0, err
	}
	s.itemLog(ctx, TaskReconcile, sum.Errors)
	return sum.Resolved, len(sum.Errors), nil
}

func (s *Scheduler) renew(ctx context.Context, _ time.Time) (int, int, error) {
	out, err := s.d.Auth.RenewDue(ctx, s.c.Batch)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, o := range out {
		if o.Err != nil {
			errs = append(errs, o.Err)
		}
	}
	s.itemLog(ctx, TaskRenew, errs)
	return len(out) - len(errs), len(errs), nil
}

func (s *Scheduler) dormant(ctx context.Context, _ time.Time) (int, int, error) {
	n, err := s.d.Auth.PauseDormant(ctx, s.c.Batch)
	return n, 0, err
}

// sweepState is the in-memory cursor of the token sweep: the last user id of
// the previous page, when each user was last swept, and which users the
// current pass has seen (to forget users that are gone when it wraps).
type sweepState struct {
	after string
	last  map[string]time.Time
	seen  map[string]bool
}

func newSweepState() sweepState {
	return sweepState{last: map[string]time.Time{}, seen: map[string]bool{}}
}

// sweepPage visits the next page of users in id order and sweeps each one
// whose last sweep is at least SweepInterval ago; the others are skipped
// without a call. After the last page the cursor wraps. A user without a
// valid credential is skipped (enrolment or the renewal deals with it). The
// sweep time is recorded before the call, so a failing user is retried after
// the interval, not on every page.
func (s *Scheduler) sweepPage(ctx context.Context, now time.Time) (int, int, error) {
	var users []store.User
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		users, err = tx.ListUsers(s.sweep.after, s.c.Batch)
		return err
	}); err != nil {
		return 0, 0, fmt.Errorf("scheduler: list users: %w", err)
	}
	st := &s.sweep
	swept, skipped, noCred := 0, 0, 0
	var errs []error
	for i, u := range users {
		if err := ctx.Err(); err != nil {
			if i > 0 {
				st.after = users[i-1].ID // resume after the last visited user
			}
			s.itemLog(ctx, TaskSweep, errs)
			return swept, len(errs), err
		}
		st.seen[u.ID] = true
		if last, ok := st.last[u.ID]; ok && now.Sub(last) < s.c.SweepInterval {
			skipped++
			continue
		}
		st.last[u.ID] = now
		res, err := s.d.Auth.Sweep(ctx, u.ID)
		switch {
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
	}
	if len(users) < s.c.Batch {
		s.wrapSweep()
	} else {
		st.after = users[len(users)-1].ID
	}
	s.itemLog(ctx, TaskSweep, errs)
	if swept+len(errs)+noCred > 0 {
		s.d.Log.Debug("scheduler: sweep page", "task", TaskSweep, "swept", swept, "skipped", skipped, "no_credential", noCred)
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

// trackedSweeps is for tests: how many users the sweep remembers.
func (s *Scheduler) trackedSweeps() int { return len(s.sweep.last) }
