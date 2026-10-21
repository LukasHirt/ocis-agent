package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// SSEConfig holds the settings of the notification-stream accelerator. Zero
// durations take the defaults; negative values are refused.
type SSEConfig struct {
	Enabled          bool
	MaxSubscriptions int           // streams open at once (default 200); users beyond rely on the poll
	MinBackoff       time.Duration // first wait before a reconnect (default 1s)
	MaxBackoff       time.Duration // longest wait, and the wait after a 401 (default 5m)
	StableAfter      time.Duration // a stream that lasted this long resets the backoff (default 1m)
	PokeGap          time.Duration // the shortest time between two polls an event brings forward (default 30s)
}

func (c SSEConfig) withDefaults() (SSEConfig, error) {
	if c.MaxSubscriptions < 0 || c.MinBackoff < 0 || c.MaxBackoff < 0 || c.StableAfter < 0 || c.PokeGap < 0 {
		return c, errors.New("scheduler: negative event stream config value")
	}
	if c.MaxSubscriptions == 0 {
		c.MaxSubscriptions = 200
	}
	if c.MinBackoff == 0 {
		c.MinBackoff = time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.StableAfter == 0 {
		c.StableAfter = time.Minute
	}
	if c.PokeGap == 0 {
		c.PokeGap = 30 * time.Second
	}
	if c.MinBackoff > c.MaxBackoff {
		return c, errors.New("scheduler: event stream min backoff above the max backoff")
	}
	return c, nil
}

// The events of the user's own file operations (measured on oCIS); their
// data (ids and an ETag, no path) is never read.
var fileEvents = map[string]bool{
	"folder-created":          true,
	"postprocessing-finished": true,
	"item-renamed":            true,
	"item-trashed":            true,
}

// sseHub holds one notification stream per eligible user. An event only
// marks the user (pokes); the watch poll then checks the user's folders
// sooner, with its usual debounce and gating, so a stream never starts a
// job and a missed event costs nothing but time (the poll is the backstop).
// The agent acts as the user, so its own writes send the same events (oCIS
// gives no way to tell them apart): they bring a poll forward that finds the
// baseline OwnWrites already moved.
type sseHub struct {
	mu     sync.Mutex
	subs   map[string]*sseSub
	pokes  map[string]bool
	closed bool
	base   context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup

	received atomic.Int64 // events read (tests)
	connects atomic.Int64 // stream requests made (tests)
}

type sseSub struct {
	key    string // the credentials it was opened with (the token label)
	cancel context.CancelFunc
	done   chan struct{}
}

func newSSEHub() *sseHub {
	ctx, cancel := context.WithCancel(context.Background())
	return &sseHub{subs: map[string]*sseSub{}, pokes: map[string]bool{}, base: ctx, stop: cancel}
}

func (h *sseHub) poke(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pokes[user] = true
}

// takePokes returns the poked users and forgets them.
func (h *sseHub) takePokes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.pokes))
	for u := range h.pokes {
		out = append(out, u)
	}
	clear(h.pokes)
	return out
}

func (h *sseHub) running() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// close stops every stream and waits for them; nothing starts afterwards.
func (h *sseHub) close() {
	h.mu.Lock()
	h.closed = true
	subs := h.subs
	h.subs = map[string]*sseSub{}
	h.mu.Unlock()
	h.stop()
	for _, s := range subs {
		s.cancel()
	}
	h.wg.Wait()
}

// Close stops the notification streams and waits until they have ended. Run
// calls it when it returns; a caller driving Tick itself calls it at the end.
func (s *Scheduler) Close() {
	if s.sse != nil {
		s.sse.close()
	}
}

// sseRun keeps one stream open for each eligible user (enrolled, proactive
// work on, not paused, with watch folders, and a valid app token), the first
// MaxSubscriptions of them in user id order, and closes the others. A stream
// is reopened when the user's token changed (renewal or re-enrolment). While
// the kill switch is on no stream is open. It returns the open streams.
func (s *Scheduler) sseRun(ctx context.Context, _ time.Time) (int, int, error) {
	h := s.sse
	want := map[string]string{}
	kill := s.c.Proactive.Limits.KillSwitchEnv
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		on, err := store.KillSwitchOn(tx)
		if err != nil || on || kill {
			kill = kill || on
			return err
		}
		after := ""
		for len(want) < s.c.SSE.MaxSubscriptions {
			ids, err := tx.ListWatchUsers(after, s.c.Batch)
			if err != nil {
				return err
			}
			for _, id := range ids {
				after = id
				tok, err := tx.GetAppToken(id)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if tok.Status != store.TokenValid || len(tok.Sealed.Ciphertext) == 0 {
					continue
				}
				want[id] = tok.Label
				if len(want) >= s.c.SSE.MaxSubscriptions {
					break
				}
			}
			if len(ids) < s.c.Batch {
				break
			}
		}
		return nil
	}); err != nil {
		return h.running(), 0, fmt.Errorf("scheduler: event stream users: %w", err)
	}
	if kill {
		clear(want)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, 0, nil
	}
	var stopped []*sseSub
	for id, sub := range h.subs {
		if key, ok := want[id]; !ok || key != sub.key {
			stopped = append(stopped, sub)
			delete(h.subs, id)
		}
	}
	h.mu.Unlock()
	for _, sub := range stopped {
		sub.cancel()
		<-sub.done // the old stream ends before a new one for the user opens
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, 0, nil
	}
	for id, key := range want {
		if _, ok := h.subs[id]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(h.base)
		sub := &sseSub{key: key, cancel: cancel, done: make(chan struct{})}
		h.subs[id] = sub
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer close(sub.done)
			s.stream(ctx, id)
		}()
	}
	return len(h.subs), 0, nil
}

// stream keeps the user's stream open until ctx ends: it builds the
// environment (fresh credentials) for every connection, pokes the user for
// every file event and reconnects after a failure or a close with an
// exponential backoff and jitter. A 401 is reported to
// Auth.HandleUnauthorized and waits the longest. Nothing about the events,
// the credentials or the errors is logged.
func (s *Scheduler) stream(ctx context.Context, user string) {
	c, h := s.c.SSE, s.sse
	backoff := c.MinBackoff
	for ctx.Err() == nil {
		wait := backoff
		env, err := s.d.Env(ctx, user)
		if err == nil {
			h.connects.Add(1)
			start := time.Now()
			err = env.Files.StreamEvents(ctx, env.Creds, "", func(e ocisclient.Event) error {
				h.received.Add(1)
				if fileEvents[e.Name] {
					h.poke(user)
				}
				return nil
			})
			if ctx.Err() != nil {
				return
			}
			if time.Since(start) >= c.StableAfter {
				backoff, wait = c.MinBackoff, c.MinBackoff
			}
			if errors.Is(err, ocisclient.ErrUnauthorized) {
				_ = s.d.Auth.HandleUnauthorized(ctx, user)
				wait = c.MaxBackoff
			}
		}
		if !sleep(ctx, jitter(wait)) {
			return
		}
		backoff = min(backoff*2, c.MaxBackoff)
	}
}

// jitter returns a duration from d/2 to d.
func jitter(d time.Duration) time.Duration {
	if d < 2 {
		return d
	}
	return d/2 + rand.N(d/2+1)
}

// sleep waits d or until ctx ends; it reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
