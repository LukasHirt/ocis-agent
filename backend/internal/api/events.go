package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// The event stream: GET /agent-api/v1/events, text/event-stream.
//
// It tells the UI WHAT to refetch, never what changed: the first frame is
// `event: ready` with `data: {"v":1}`; then the handler reads the user's
// store.ChangeMarks every eventsPollEvery (one short read transaction per
// poll, no reader held in between) and, when any mark differs from the
// previous poll (in either direction: deleting a conversation can lower a
// mark), writes `event: changed` with `data: {"kinds":[...]}` listing, in
// this order, "jobs" (JobsAt), "messages" (MessagesAt), "activity" (AuditSeq;
// approvals, the audit log and the status) and "settings" (PolicyAt). A
// comment `: ping` every eventsPingEvery keeps proxies from closing the
// connection. A payload never carries ids, content, titles or error text.
//
// There are no `id:` fields and Last-Event-ID is ignored: after a reconnect
// the client refetches everything. The marks have millisecond resolution,
// so a change committed in the same millisecond as the previous mark is
// reported only with the next change; clients therefore also refresh fully on
// a slow timer (60 s).
//
// The stream ends after eventsLifetime (`event: bye`, `data:
// {"reason":"lifetime"}`; clients reconnect), when Deps.Stop is closed
// (reason "shutdown"), when the client goes away, when a write or flush
// fails (each frame must be written within eventsWriteDeadline) and when a
// poll fails (the client reconnects). Streams are capped per user
// (Settings.StreamsPerUser) and in the process (Settings.StreamsTotal); over
// a cap the answer is 429 rate_limited with Retry-After: 5. HEAD is 405 and
// no query parameter is accepted.
const (
	eventsPollEvery     = 2 * time.Second
	eventsPingEvery     = 25 * time.Second
	eventsLifetime      = time.Hour
	eventsWriteDeadline = 15 * time.Second
	eventsRetryAfter    = 5 * time.Second
	// eventsPollTimeout bounds one poll's read transaction
	eventsPollTimeout = 5 * time.Second
	// eventsRouteTimeout is the stream route's context deadline, a backstop
	// behind the lifetime timer
	eventsRouteTimeout = eventsLifetime + time.Minute

	defaultStreamsPerUser = 3
	defaultStreamsTotal   = 200
)

const (
	frameReady         = "event: ready\ndata: {\"v\":1}\n\n"
	framePing          = ": ping\n\n"
	frameByeLifetime   = "event: bye\ndata: {\"reason\":\"lifetime\"}\n\n"
	frameByeShutdown   = "event: bye\ndata: {\"reason\":\"shutdown\"}\n\n"
	frameChangedPrefix = "event: changed\ndata: {\"kinds\":["
	frameChangedSuffix = "]}\n\n"
)

// eventTimers are the timers of one stream; a field of the server, so tests
// drive them.
type eventTimers struct {
	poll, ping, lifetime <-chan time.Time
	stop                 func()
	afterPoll            func() // tests only: runs after every poll
}

// eventTimersEvery returns the real timers with a poll every d (0 or less:
// eventsPollEvery).
func eventTimersEvery(d time.Duration) func(string) eventTimers {
	if d <= 0 {
		d = eventsPollEvery
	}
	return func(string) eventTimers { return realEventTimers(d) }
}

func realEventTimers(pollEvery time.Duration) eventTimers {
	poll := time.NewTicker(pollEvery)
	ping := time.NewTicker(eventsPingEvery)
	life := time.NewTimer(eventsLifetime)
	return eventTimers{poll: poll.C, ping: ping.C, lifetime: life.C, stop: func() {
		poll.Stop()
		ping.Stop()
		life.Stop()
	}}
}

// streamSlots counts the open streams per user and in total.
type streamSlots struct {
	maxUser, maxTotal int

	mu      sync.Mutex
	perUser map[string]int
	total   int
}

func newStreamSlots(maxUser, maxTotal int) *streamSlots {
	return &streamSlots{maxUser: maxUser, maxTotal: maxTotal, perUser: map[string]int{}}
}

func (s *streamSlots) acquire(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= s.maxTotal || s.perUser[user] >= s.maxUser {
		return false
	}
	s.total++
	s.perUser[user]++
	return true
}

func (s *streamSlots) release(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.perUser[user]
	if !ok {
		return
	}
	if n <= 1 {
		delete(s.perUser, user)
	} else {
		s.perUser[user] = n - 1
	}
	s.total--
}

func (s *streamSlots) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func (s *streamSlots) openFor(user string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.perUser[user]
}

var errStopping = newAPIError(http.StatusServiceUnavailable, "unavailable")

// changedKinds lists the kinds whose marks differ, in the fixed order.
func changedKinds(prev, cur store.ChangeMarks) []string {
	var k []string
	if prev.JobsAt != cur.JobsAt {
		k = append(k, "jobs")
	}
	if prev.MessagesAt != cur.MessagesAt {
		k = append(k, "messages")
	}
	if prev.AuditSeq != cur.AuditSeq {
		k = append(k, "activity")
	}
	if prev.PolicyAt != cur.PolicyAt {
		k = append(k, "settings")
	}
	return k
}

func changedFrameOf(kinds []string) []byte {
	b := []byte(frameChangedPrefix)
	for i, k := range kinds {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(append(append(b, '"'), k...), '"')
	}
	return append(b, frameChangedSuffix...)
}

// marks reads the user's change marks in one short read transaction.
func (s *Server) marks(ctx context.Context, userID string) (store.ChangeMarks, error) {
	ctx, cancel := context.WithTimeout(ctx, eventsPollTimeout)
	defer cancel()
	var m store.ChangeMarks
	err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		m, err = tx.ChangeMarks(userID)
		return err
	})
	return m, err
}

// stopping reports whether Deps.Stop is closed.
func (s *Server) stopping() bool {
	select {
	case <-s.d.Stop:
		return true
	default:
		return false
	}
}

// streamEvents is GET /agent-api/v1/events (see the top of this file).
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	if refuseHEAD(w, r) {
		return nil
	}
	if _, err := query(r); err != nil {
		return err
	}
	if s.stopping() {
		return errStopping
	}
	if !s.streams.acquire(p.UserID) {
		rateLimited(w, r, eventsRetryAfter)
		return nil
	}
	defer s.streams.release(p.UserID)
	ctx := r.Context()
	prev, err := s.marks(ctx, p.UserID)
	if err != nil {
		return err
	}
	timers := s.eventTimers(p.UserID)
	defer timers.stop()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// write sends one frame within the write deadline; false ends the stream
	// (ErrNotSupported only where there is no connection: test recorders)
	write := func(frame []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(s.eventWriteDeadline)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if _, err := w.Write(frame); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write([]byte(frameReady)) {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.d.Stop:
			write([]byte(frameByeShutdown))
			return nil
		case <-timers.lifetime:
			write([]byte(frameByeLifetime))
			return nil
		case <-timers.ping:
			if !write([]byte(framePing)) {
				return nil
			}
		case <-timers.poll:
			cur, err := s.marks(ctx, p.UserID)
			if err != nil {
				// the response has started: end the stream, the client
				// reconnects (and gets a normal error while the store fails)
				if ri := infoFrom(ctx); ri != nil && ctx.Err() == nil {
					ri.category = "poll_failed"
				}
				return nil
			}
			if kinds := changedKinds(prev, cur); len(kinds) > 0 {
				prev = cur
				if !write(changedFrameOf(kinds)) {
					return nil
				}
			}
			if timers.afterPoll != nil {
				timers.afterPoll()
			}
		}
	}
}
