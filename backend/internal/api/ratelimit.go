package api

import (
	"container/list"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// maxLimiterKeys bounds the number of buckets a limiter keeps.
const maxLimiterKeys = 10000

// The pre-auth bucket per client IP counts FAILED authentications only
// (missing or malformed bearer, a token oCIS rejects); successful requests
// never touch it. When it is empty, a request whose token the validator has
// cached still proceeds and only the per-user limit applies; anything else
// is refused before the validator is called. So a flood of bad tokens from
// the reverse proxy's address (all users arrive from it, X-Forwarded-For is
// not trusted) cannot lock out users who are already signed in, and cannot
// turn into a flood of Graph calls.
//
// This is not flood protection: rate limiting all requests per real client
// must also exist at the reverse proxy. Trusting X-Forwarded-For from listed
// proxies (AGENT_TRUSTED_PROXIES) is future work.
//
// The failure bucket gets these multiples of the per-user rate and burst,
// since every user behind the proxy shares it.
const (
	ipRateFactor  = 10
	ipBurstFactor = 10
)

// sweepEvery is how often a full limiter may scan all its buckets.
const sweepEvery = time.Second

// limiter is a token bucket per key with a bounded number of keys.
type limiter struct {
	rate  float64 // tokens per second
	burst float64
	max   int
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*list.Element // of *bucket
	lru       *list.List               // front: most recently used
	lastSweep time.Time
	sweeps    int // full scans so far (tests)
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time // last refill
}

func newLimiter(rate float64, burst, max int, now func() time.Time) *limiter {
	return &limiter{rate: rate, burst: float64(burst), max: max, now: now,
		buckets: map[string]*list.Element{}, lru: list.New()}
}

// allow takes a token for key. If none is left it reports how long until the
// next one.
func (l *limiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key, now)
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.wait(b)
}

// blocked reports whether key has no token left, without taking one or
// creating a bucket.
func (l *limiter) blocked(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.buckets[key]
	if !ok {
		return false, 0
	}
	b := e.Value.(*bucket)
	l.refill(b, now)
	if b.tokens >= 1 {
		return false, 0
	}
	return true, l.wait(b)
}

// charge takes a token for key if one is left (a failed attempt).
func (l *limiter) charge(key string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key, now)
	b.tokens = math.Max(0, b.tokens-1)
}

// get returns the refilled bucket of key, creating it (and making room) if
// needed, and marks it most recently used.
func (l *limiter) get(key string, now time.Time) *bucket {
	if e, ok := l.buckets[key]; ok {
		l.lru.MoveToFront(e)
		b := e.Value.(*bucket)
		l.refill(b, now)
		return b
	}
	if len(l.buckets) >= l.max {
		l.evict(now)
	}
	b := &bucket{key: key, tokens: l.burst, last: now}
	l.buckets[key] = l.lru.PushFront(b)
	return b
}

func (l *limiter) wait(b *bucket) time.Duration {
	return time.Duration(math.Ceil((1 - b.tokens) / l.rate * float64(time.Second)))
}

func (l *limiter) refill(b *bucket, now time.Time) {
	if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el.Seconds()*l.rate)
		b.last = now
	}
}

// evict makes room for one key. At most once per sweepEvery it drops every
// bucket that would have refilled completely (indistinguishable from a new
// one, so nothing is lost); if that frees nothing, or a sweep ran less than
// sweepEvery ago, the least recently used bucket goes (O(1)).
func (l *limiter) evict(now time.Time) {
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= sweepEvery {
		l.lastSweep = now
		l.sweeps++
		for k, e := range l.buckets {
			b := e.Value.(*bucket)
			tokens := b.tokens
			if el := now.Sub(b.last); el > 0 {
				tokens += el.Seconds() * l.rate
			}
			if tokens >= l.burst {
				l.lru.Remove(e)
				delete(l.buckets, k)
			}
		}
	}
	if len(l.buckets) >= l.max {
		if e := l.lru.Back(); e != nil {
			l.lru.Remove(e)
			delete(l.buckets, e.Value.(*bucket).key)
		}
	}
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *limiter) has(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.buckets[key]
	return ok
}

// clientKey is the pre-auth limiter's key: the RemoteAddr host (never
// X-Forwarded-For), IPv4-mapped addresses as IPv4 and IPv6 by its /64, the
// smallest block a single client usually gets.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}
