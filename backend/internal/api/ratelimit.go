package api

import (
	"math"
	"sync"
	"time"
)

// maxLimiterKeys bounds the number of buckets a limiter keeps.
const maxLimiterKeys = 10000

// The pre-auth limiter is keyed by the client IP. Behind the reverse proxy
// in front of oCIS every user arrives from the proxy's address, so it gets
// this many times the per-user rate and burst; it exists to protect the
// token validation (a Graph call per unknown token), the per-user limit is
// the one that applies to a single user.
const (
	ipRateFactor  = 10
	ipBurstFactor = 10
)

// limiter is a token bucket per key with a bounded number of keys.
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	max   int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time // last refill, which is also the last use
}

func newLimiter(rate float64, burst, max int, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: float64(burst), max: max, now: now, buckets: map[string]*bucket{}}
}

// allow takes a token for key. If none is left it reports how long until the
// next one.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		l.refill(b, now)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration(math.Ceil((1 - b.tokens) / l.rate * float64(time.Second)))
	return false, wait
}

func (l *limiter) refill(b *bucket, now time.Time) {
	if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el.Seconds()*l.rate)
		b.last = now
	}
}

// evict makes room for one key. Buckets that would have refilled completely
// are indistinguishable from new ones, so they go first and losslessly; if
// none is full, the least recently used bucket goes. Buckets are only
// refilled when used, so last is the time of the last use.
func (l *limiter) evict(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		tokens := b.tokens
		if el := now.Sub(b.last); el > 0 {
			tokens += el.Seconds() * l.rate
		}
		if tokens >= l.burst {
			delete(l.buckets, k)
			continue
		}
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = k, b.last
		}
	}
	if len(l.buckets) >= l.max && oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
