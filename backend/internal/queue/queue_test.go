package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
	"github.com/owncloud/ocis-agent/backend/internal/store/storetest"
)

func newStore(t *testing.T, opts ...sqlite.Option) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addUser(t *testing.T, st store.Store, id string) {
	t.Helper()
	err := st.InTx(context.Background(), func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: id, Name: id, PersonalDriveID: "drive-" + id})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func job(user string) store.Job {
	return store.Job{UserID: user, TriggerKind: "chat", Goal: "g"}
}

type result struct {
	claim store.Claim
	err   error
}

func claimAsync(ctx context.Context, q *Queue, worker string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		c, err := q.Claim(ctx, worker)
		ch <- result{c, err}
	}()
	return ch
}

func within(t *testing.T, ch <-chan result, d time.Duration, what string) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("%s: no result within %s", what, d)
		return result{}
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{
		-1: 30 * time.Second, 0: 30 * time.Second, 1: 30 * time.Second, 2: time.Minute,
		3: 2 * time.Minute, 5: 8 * time.Minute, 6: 10 * time.Minute, 20: 10 * time.Minute,
	}
	for attempt, want := range cases {
		if got := Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestEnqueueThenClaim(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	q := New(st)
	j, err := q.Enqueue(context.Background(), job("u1"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := q.Claim(ctx, "w1")
	if err != nil || c.Job.ID != j.ID || c.Fence != 1 {
		t.Fatalf("Claim = %+v, %v", c, err)
	}
}

func TestEnqueueRejectsAnInvalidJob(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	if _, err := New(st).Enqueue(context.Background(), store.Job{UserID: "u1"}); err == nil {
		t.Fatal("Enqueue accepted a job without a goal")
	}
}

func TestClaimBlocksUntilEnqueueWakesIt(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	q := New(st, WithPollInterval(time.Hour)) // only the wake-up can end the wait
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := claimAsync(ctx, q, "w1")

	time.Sleep(50 * time.Millisecond)
	select {
	case r := <-ch:
		t.Fatalf("Claim returned before any job existed: %+v", r)
	default:
	}
	j, err := q.Enqueue(context.Background(), job("u1"))
	if err != nil {
		t.Fatal(err)
	}
	r := within(t, ch, 2*time.Second, "claim after enqueue")
	if r.err != nil || r.claim.Job.ID != j.ID {
		t.Fatalf("result = %+v", r)
	}
}

func TestClaimFallsBackToPolling(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	q := New(st, WithPollInterval(20*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := claimAsync(ctx, q, "w1")

	time.Sleep(50 * time.Millisecond)
	// insert behind the queue's back: no Wake
	err := st.InTx(context.Background(), func(tx store.Tx) error {
		_, err := tx.InsertJob(job("u1"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := within(t, ch, 2*time.Second, "claim via polling"); r.err != nil {
		t.Fatalf("result = %+v", r)
	}
}

func TestClaimReturnsWhenTheContextEnds(t *testing.T) {
	st := newStore(t)
	q := New(st)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := q.Claim(ctx, "w1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestOneEnqueueBurstWakesAllWaiters(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	addUser(t, st, "u2")
	q := New(st, WithPollInterval(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := claimAsync(ctx, q, "w1")
	b := claimAsync(ctx, q, "w2")
	time.Sleep(50 * time.Millisecond)

	if _, err := q.Enqueue(context.Background(), job("u1")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(context.Background(), job("u2")); err != nil {
		t.Fatal(err)
	}
	ra := within(t, a, 2*time.Second, "first waiter")
	rb := within(t, b, 2*time.Second, "second waiter")
	if ra.err != nil || rb.err != nil || ra.claim.Job.UserID == rb.claim.Job.UserID {
		t.Fatalf("results = %+v / %+v", ra, rb)
	}
}

func TestReleaseWakesAWaiterForTheSameUser(t *testing.T) {
	st := newStore(t)
	addUser(t, st, "u1")
	q := New(st, WithPollInterval(time.Hour))
	bg := context.Background()
	if _, err := q.Enqueue(bg, job("u1")); err != nil {
		t.Fatal(err)
	}
	second, err := q.Enqueue(bg, job("u1"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.Claim(bg, "w1")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	ch := claimAsync(ctx, q, "w2") // blocked: u1 holds the lease
	time.Sleep(50 * time.Millisecond)
	if applied, err := q.Release(bg, first, store.JobSucceeded, nil, ""); err != nil || !applied {
		t.Fatalf("Release = %v, %v", applied, err)
	}
	r := within(t, ch, 2*time.Second, "claim after release")
	if r.err != nil || r.claim.Job.ID != second.ID {
		t.Fatalf("result = %+v", r)
	}
}

func TestRenewAndReap(t *testing.T) {
	clk := storetest.NewClock()
	st := newStore(t, sqlite.WithClock(clk.Now))
	addUser(t, st, "u1")
	q := New(st, WithLeaseTTL(time.Minute))
	bg := context.Background()
	j, err := q.Enqueue(bg, job("u1"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := q.Claim(bg, "w1")
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(45 * time.Second)
	renewed, err := q.Renew(bg, c)
	if err != nil || !renewed.ExpiresAt.Equal(clk.Now().Add(time.Minute)) {
		t.Fatalf("Renew = %+v, %v", renewed, err)
	}
	clk.Advance(45 * time.Second) // 90s after the claim, 45s after the renewal
	if r, err := q.Reap(bg, 3); err != nil || r.Requeued != 0 || r.Failed != 0 {
		t.Fatalf("Reap of a renewed lease = %+v, %v", r, err)
	}
	clk.Advance(30 * time.Second) // the renewed lease has now expired
	r, err := q.Reap(bg, 3)
	if err != nil || r.Requeued != 1 {
		t.Fatalf("Reap = %+v, %v", r, err)
	}
	var got store.Job
	_ = st.View(bg, func(tx store.Tx) error {
		var err error
		got, err = tx.GetJob("u1", j.ID)
		return err
	})
	if got.State != store.JobQueued || !got.NotBefore.Equal(clk.Now().Add(Backoff(1))) {
		t.Fatalf("job after reap = %+v", got)
	}
	if _, err := q.Renew(bg, c); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("a reaped worker could renew: err = %v", err)
	}
}
