// Package auth validates users, enrols them, and runs the lifecycle of the
// agent's app token.
package auth

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

// Errors returned by the validator.
var (
	// ErrUnauthenticated: oCIS does not accept the token (or none was given).
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrNoLoginName: Graph returned no onPremisesSamAccountName, which
	// app-token Basic auth needs.
	ErrNoLoginName = errors.New("auth: the user has no login name")
)

// Identity is who a bearer token belongs to.
type Identity struct {
	UserID      string // Graph id
	Username    string // login name, for app-token Basic auth
	DisplayName string
	DriveID     string // personal space
}

// Validator checks bearer tokens by asking oCIS itself (Graph /me): a token is
// valid exactly when oCIS accepts it, which is the same authority that will
// authorise the agent's later calls.
type Validator struct {
	c          *ocisclient.Client
	ttl        time.Duration
	now        func() time.Time
	maxEntries int

	mu    sync.Mutex
	cache map[[32]byte]cacheEntry

	// known holds the SHA-256 of tokens oCIS accepted, for longer than the
	// cache (knownTTL or the token's exp), least recently used first out.
	// Only a successful validation adds an entry and a rejection removes it,
	// so a client cannot put a token into it.
	// At most maxKnownPerUser entries per user id, the newest, so one user
	// minting many valid tokens cannot push the other users out.
	maxKnown     int
	known        map[[32]byte]*list.Element // of *knownEntry
	knownLRU     *list.List
	knownPerUser map[string]int

	// flights coalesces concurrent validations of one token: one round trip
	// to oCIS, the others wait for its result.
	flights map[[32]byte]*flight
}

// flight is a validation in progress.
type flight struct {
	done chan struct{} // closed when id and err are set
	id   Identity
	err  error
}

// knownTTL is the longest a token stays known without a new validation.
const knownTTL = 12 * time.Hour

// maxKnownPerUser caps the known tokens of one user (several browsers and
// devices, each with its own token).
const maxKnownPerUser = 6

type knownEntry struct {
	key   [32]byte
	user  string
	until time.Time
}

type cacheEntry struct {
	id    Identity
	until time.Time
}

// ValidatorOption configures NewValidator.
type ValidatorOption func(*Validator)

// WithCacheTTL sets how long a successful validation is reused (default 60s).
func WithCacheTTL(d time.Duration) ValidatorOption { return func(v *Validator) { v.ttl = d } }

// WithValidatorClock sets the clock (for tests).
func WithValidatorClock(now func() time.Time) ValidatorOption {
	return func(v *Validator) { v.now = now }
}

// NewValidator returns a validator over c.
func NewValidator(c *ocisclient.Client, opts ...ValidatorOption) *Validator {
	v := &Validator{c: c, ttl: time.Minute, now: time.Now, maxEntries: 4096, cache: map[[32]byte]cacheEntry{},
		maxKnown: 4096, known: map[[32]byte]*list.Element{}, knownLRU: list.New(), knownPerUser: map[string]int{},
		flights: map[[32]byte]*flight{}}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Validate returns the identity of the bearer token. Failures of oCIS itself
// (network, 5xx) are returned as they are and never cached; only a rejected
// token is ErrUnauthenticated. Concurrent calls for one uncached token share
// one validation; a caller whose context ends stops waiting, and a waiter is
// not failed by the cancellation of the caller that ran the validation (it
// tries again).
func (v *Validator) Validate(ctx context.Context, token ocisclient.Secret) (Identity, error) {
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token.Reveal()))
	for {
		now := v.now()
		v.mu.Lock()
		if e, ok := v.cache[key]; ok && now.Before(e.until) {
			v.mu.Unlock()
			return e.id, nil
		}
		if f, ok := v.flights[key]; ok {
			v.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return Identity{}, ctx.Err()
			}
			if isContextErr(f.err) && ctx.Err() == nil {
				continue // the leader's own context ended, not ours
			}
			return f.id, f.err
		}
		f := &flight{done: make(chan struct{})}
		v.flights[key] = f
		v.mu.Unlock()
		f.id, f.err = v.validate(ctx, token, key, now)
		v.mu.Lock()
		delete(v.flights, key)
		v.mu.Unlock()
		close(f.done)
		return f.id, f.err
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// validate asks oCIS (Graph /me and the personal drive) and records the
// result in the cache and the known set.
func (v *Validator) validate(ctx context.Context, token ocisclient.Secret, key [32]byte, now time.Time) (Identity, error) {
	cr := ocisclient.Bearer(token)
	me, err := v.c.Me(ctx, cr)
	if err != nil {
		if errors.Is(err, ocisclient.ErrUnauthorized) || errors.Is(err, ocisclient.ErrForbidden) {
			v.forget(key)
			return Identity{}, ErrUnauthenticated
		}
		return Identity{}, err
	}
	if me.Username == "" {
		return Identity{}, ErrNoLoginName
	}
	drive, err := v.c.PersonalDriveID(ctx, cr)
	if err != nil {
		// oCIS refusing the drive lookup of the token it just accepted for
		// /me is a rejection of the token (revoked in between), not an outage
		if errors.Is(err, ocisclient.ErrUnauthorized) || errors.Is(err, ocisclient.ErrForbidden) {
			v.forget(key)
			return Identity{}, ErrUnauthenticated
		}
		return Identity{}, err
	}
	id := Identity{UserID: me.ID, Username: me.Username, DisplayName: me.DisplayName, DriveID: drive}

	exp, hasExp := tokenExpiry(token.Reveal())
	until := now.Add(v.ttl)
	if hasExp && exp.Before(until) {
		until = exp
	}
	if until.After(now) {
		v.store(key, id, until)
	}
	knownUntil := now.Add(knownTTL)
	if hasExp && exp.Before(knownUntil) {
		knownUntil = exp
	}
	if knownUntil.After(now) {
		v.remember(key, id.UserID, knownUntil)
	}
	return id, nil
}

// Known reports whether oCIS accepted the token in the last knownTTL (and
// before its exp) and has not rejected it since. It never calls oCIS. A
// known token is not authenticated by this: the caller must still Validate
// it; Known only says the token belonged to a real user recently.
func (v *Validator) Known(token ocisclient.Secret) bool {
	if token == "" {
		return false
	}
	key := sha256.Sum256([]byte(token.Reveal()))
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.known[key]
	if !ok {
		return false
	}
	if !now.Before(e.Value.(*knownEntry).until) {
		v.dropKnown(e)
		return false
	}
	v.knownLRU.MoveToFront(e)
	return true
}

// remember marks a validated token of user as known until the given time.
// A user over maxKnownPerUser loses their oldest entry, the whole set over
// maxKnown its least recently used one.
func (v *Validator) remember(key [32]byte, user string, until time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.known[key]; ok {
		ke := e.Value.(*knownEntry)
		if ke.user != user {
			v.knownPerUser[ke.user]--
			v.knownPerUser[user]++
			ke.user = user
		}
		ke.until = until
		v.knownLRU.MoveToFront(e)
		return
	}
	if v.knownPerUser[user] >= maxKnownPerUser {
		for e := v.knownLRU.Back(); e != nil; e = e.Prev() {
			if e.Value.(*knownEntry).user == user {
				v.dropKnown(e)
				break
			}
		}
	}
	for len(v.known) >= v.maxKnown {
		v.dropKnown(v.knownLRU.Back())
	}
	v.known[key] = v.knownLRU.PushFront(&knownEntry{key: key, user: user, until: until})
	v.knownPerUser[user]++
}

// dropKnown removes an entry; the caller holds v.mu.
func (v *Validator) dropKnown(e *list.Element) {
	ke := e.Value.(*knownEntry)
	v.knownLRU.Remove(e)
	delete(v.known, ke.key)
	if v.knownPerUser[ke.user]--; v.knownPerUser[ke.user] <= 0 {
		delete(v.knownPerUser, ke.user)
	}
}

// forget drops a token oCIS rejected.
func (v *Validator) forget(key [32]byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.known[key]; ok {
		v.dropKnown(e)
	}
}

func (v *Validator) knownSize() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.known)
}

// Cached returns the identity of a token that Validate accepted and that is
// still cached. It never calls oCIS, so it is cheap enough for a caller that
// must decide without a network call (the API's pre-auth rate limit).
func (v *Validator) Cached(token ocisclient.Secret) (Identity, bool) {
	if token == "" {
		return Identity{}, false
	}
	key := sha256.Sum256([]byte(token.Reveal()))
	now := v.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if e, ok := v.cache[key]; ok && now.Before(e.until) {
		return e.id, true
	}
	return Identity{}, false
}

// store caches id until the given time, keeping the cache bounded.
func (v *Validator) store(key [32]byte, id Identity, until time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= v.maxEntries {
		now := v.now()
		for k, e := range v.cache {
			if !now.Before(e.until) {
				delete(v.cache, k)
			}
		}
		for k := range v.cache {
			if len(v.cache) < v.maxEntries {
				break
			}
			delete(v.cache, k)
		}
	}
	v.cache[key] = cacheEntry{id: id, until: until}
}

func (v *Validator) size() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.cache)
}

// tokenExpiry reads the exp claim of a JWT WITHOUT verifying it. It is only
// ever used to shorten how long a validation is cached, never to accept a
// token.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0).UTC(), true
}
