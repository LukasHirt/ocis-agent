package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// maxRequeueDelay caps the backoff of a requeue.
const maxRequeueDelay = 10 * time.Minute

// requeueDelay doubles base per run after the first, up to maxRequeueDelay.
func requeueDelay(base time.Duration, runs int) time.Duration {
	d := base
	for i := 1; i < runs && d < maxRequeueDelay; i++ {
		d *= 2
	}
	return min(d, maxRequeueDelay)
}

// ending describes a terminal release. Reason must be one of the fixed Reason
// constants (or empty): it becomes Job.Error and the audit detail.
type ending struct {
	state      store.JobState
	reason     string
	checkpoint json.RawMessage // nil keeps the stored one
	step       *store.JobStep  // recorded in the same transaction, if set
	events     []audit.Event   // appended in the same transaction
	answer     string          // the final answer of a succeeded job
}

// lost reports an error that means the job is no longer ours.
func lost(err error) bool {
	return errors.Is(err, store.ErrFenceLost) || errors.Is(err, store.ErrConflict)
}

// end releases the job into a terminal state with a job_finished event and,
// for a chat job, its conversation message (recordEnd) in one transaction.
// A job that is no longer running (for example cancelled by the user) is
// left alone and is not an error; a lost fence is returned wrapped.
func (r *Runner) end(ctx context.Context, claim store.Claim, e ending) error {
	j := claim.Job
	err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
		if e.step != nil {
			if _, err := tx.AppendStep(*e.step, claim.Fence); err != nil {
				return err
			}
		}
		ok, err := tx.ReleaseJob(claim, e.state, e.checkpoint, e.reason)
		if err != nil {
			return err
		}
		if !ok {
			return errNotRunning // roll back: nothing to record
		}
		events := append(append([]audit.Event(nil), e.events...), audit.Event{Kind: audit.KindJobFinished, Outcome: string(e.state), Detail: e.reason})
		for _, ev := range events {
			ev.UserID, ev.JobID, ev.TS = j.UserID, j.ID, r.cfg.Now()
			if _, err := tx.AppendAudit(ev); err != nil {
				return err
			}
		}
		return recordEnd(tx, j, e)
	})
	if errors.Is(err, errNotRunning) || errors.Is(err, store.ErrConflict) {
		return nil // already terminal: someone else ended it
	}
	if err != nil {
		return fmt.Errorf("agent: end job: %w", err)
	}
	r.d.Queue.Wake()
	return nil
}

var errNotRunning = errors.New("agent: the job is no longer running")

func (r *Runner) fail(ctx context.Context, claim store.Claim, reason string) error {
	return r.end(ctx, claim, ending{state: store.JobFailed, reason: reason})
}

// requeue puts the job back to queued, claimable at now+delay, keeping the
// stored checkpoint.
func (r *Runner) requeue(ctx context.Context, claim store.Claim, delay time.Duration) error {
	at := r.cfg.Now().Add(delay)
	var applied bool
	err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
		var err error
		applied, err = tx.RequeueJob(claim, at, nil)
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agent: requeue job: %w", err)
	}
	if applied {
		r.d.Queue.Wake()
	}
	return nil
}

// backoff requeues after the configured delay, doubled per run.
func (r *Runner) backoff(ctx context.Context, claim store.Claim) error {
	return r.requeue(ctx, claim, requeueDelay(r.cfg.RequeueDelay, claim.Job.Runs))
}

// unauthorized reports a 401 for the app credentials and requeues the job for
// later (the report may pause the user).
func (r *Runner) unauthorized(ctx context.Context, claim store.Claim) error {
	r.reportUnauthorized(ctx, claim)
	return r.requeue(ctx, claim, r.cfg.PausedDelay)
}

// reportUnauthorized tells the credentials about a 401. A failed report is
// retried at the next 401.
func (r *Runner) reportUnauthorized(ctx context.Context, claim store.Claim) {
	_ = r.d.Creds.HandleUnauthorized(ctx, claim.Job.UserID)
}
