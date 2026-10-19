package api

import (
	"sync"
	"time"
)

// seenEvery is how often a request records the user's visit (auth.Seen, a
// database write).
const seenEvery = time.Minute

// seenThrottle remembers when Seen last succeeded per user, in memory and
// bounded. A user it cannot record (the map is full of fresh entries) is
// simply due again: Seen is then called more often, never less.
type seenThrottle struct {
	every time.Duration
	max   int
	now   func() time.Time

	mu        sync.Mutex
	last      map[string]time.Time
	lastSweep time.Time
}

func newSeenThrottle(every time.Duration, max int, now func() time.Time) *seenThrottle {
	return &seenThrottle{every: every, max: max, now: now, last: map[string]time.Time{}}
}

// due reports whether the user's visit should be recorded now.
func (s *seenThrottle) due(user string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.last[user]
	return !ok || now.Sub(t) >= s.every || now.Before(t)
}

// done records a successful Seen. Only call it after a success, so a failure
// is retried by the next request.
func (s *seenThrottle) done(user string) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.last[user]; !ok && len(s.last) >= s.max {
		if !(s.lastSweep.IsZero() || now.Sub(s.lastSweep) >= sweepEvery) {
			return
		}
		s.lastSweep = now
		for k, t := range s.last {
			if now.Sub(t) >= s.every {
				delete(s.last, k)
			}
		}
		if len(s.last) >= s.max {
			return
		}
	}
	s.last[user] = now
}

func (s *seenThrottle) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.last)
}
