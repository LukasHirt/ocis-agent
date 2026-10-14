// Package queue is a thin facade over the store's job and lease operations. It
// adds what the store cannot: an in-process wake-up so a claimer reacts to new
// work immediately, and a poll interval as the backstop for work that arrived
// by other means (another process, a reaped job coming out of backoff).
package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultLeaseTTL = 2 * time.Minute
	defaultPoll     = 2 * time.Second

	backoffBase = 30 * time.Second
	backoffMax  = 10 * time.Minute
)

// Option configures New.
type Option func(*Queue)

// WithLeaseTTL sets how long a claim's lease lasts before it must be renewed.
func WithLeaseTTL(d time.Duration) Option { return func(q *Queue) { q.ttl = d } }

// WithPollInterval sets how often a waiting Claim re-checks the store.
func WithPollInterval(d time.Duration) Option { return func(q *Queue) { q.poll = d } }

// Queue hands jobs to workers.
type Queue struct {
	st   store.Store
	ttl  time.Duration
	poll time.Duration
	wake chan struct{}
}

// New returns a Queue on st.
func New(st store.Store, opts ...Option) *Queue {
	q := &Queue{st: st, ttl: defaultLeaseTTL, poll: defaultPoll, wake: make(chan struct{}, 1)}
	for _, o := range opts {
		o(q)
	}
	return q
}

// Enqueue adds a job and wakes a waiting claimer.
func (q *Queue) Enqueue(ctx context.Context, j store.Job) (store.Job, error) {
	var out store.Job
	err := q.st.InTx(ctx, func(tx store.Tx) error {
		var err error
		out, err = tx.InsertJob(j)
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	q.Wake()
	return out, nil
}

// Wake nudges one waiting Claim to look for work now. It never blocks.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Claim blocks until a job can be claimed for worker, or ctx ends. After a
// successful claim it wakes the next waiter, so a burst of enqueues is not
// served one poll interval at a time.
func (q *Queue) Claim(ctx context.Context, worker string) (store.Claim, error) {
	timer := time.NewTimer(q.poll)
	defer timer.Stop()
	for {
		var c store.Claim
		var ok bool
		err := q.st.InTx(ctx, func(tx store.Tx) error {
			var err error
			c, ok, err = tx.ClaimNextJob(worker, q.ttl)
			return err
		})
		if err != nil {
			return store.Claim{}, err
		}
		if ok {
			q.Wake()
			return c, nil
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(q.poll)
		select {
		case <-ctx.Done():
			return store.Claim{}, ctx.Err()
		case <-q.wake:
		case <-timer.C:
		}
	}
}

// Renew extends the claim's lease by the queue's lease ttl.
func (q *Queue) Renew(ctx context.Context, c store.Claim) (store.Claim, error) {
	var out store.Claim
	err := q.st.InTx(ctx, func(tx store.Tx) error {
		var err error
		out, err = tx.RenewLease(c, q.ttl)
		return err
	})
	return out, err
}

// Release frees the claim's lease and moves the job to next. Other jobs of the
// same user become claimable, so a waiter is woken.
func (q *Queue) Release(ctx context.Context, c store.Claim, next store.JobState, checkpoint json.RawMessage, errMsg string) (bool, error) {
	var applied bool
	err := q.st.InTx(ctx, func(tx store.Tx) error {
		var err error
		applied, err = tx.ReleaseJob(c, next, checkpoint, errMsg)
		return err
	})
	if err != nil {
		return false, err
	}
	q.Wake()
	return applied, nil
}

// Reap requeues or fails jobs whose lease is gone (see store.Tx.ReapExpired),
// using Backoff between attempts.
func (q *Queue) Reap(ctx context.Context, maxAttempts int) (store.ReapResult, error) {
	var res store.ReapResult
	err := q.st.InTx(ctx, func(tx store.Tx) error {
		var err error
		res, err = tx.ReapExpired(maxAttempts, Backoff)
		return err
	})
	return res, err
}

// Backoff is the delay before attempt+1 after a failed attempt: 30s, doubling,
// capped at 10 minutes. Attempts below 1 count as 1.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase
	for i := 1; i < attempt && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}
