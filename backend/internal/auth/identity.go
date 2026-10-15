// Package auth validates users, enrols them, and runs the lifecycle of the
// agent's app token.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	v := &Validator{c: c, ttl: time.Minute, now: time.Now, maxEntries: 4096, cache: map[[32]byte]cacheEntry{}}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Validate returns the identity of the bearer token. Failures of oCIS itself
// (network, 5xx) are returned as they are and never cached; only a rejected
// token is ErrUnauthenticated.
func (v *Validator) Validate(ctx context.Context, token ocisclient.Secret) (Identity, error) {
	if token == "" {
		return Identity{}, ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token.Reveal()))
	now := v.now()
	v.mu.Lock()
	if e, ok := v.cache[key]; ok && now.Before(e.until) {
		v.mu.Unlock()
		return e.id, nil
	}
	v.mu.Unlock()

	cr := ocisclient.Bearer(token)
	me, err := v.c.Me(ctx, cr)
	if err != nil {
		if errors.Is(err, ocisclient.ErrUnauthorized) || errors.Is(err, ocisclient.ErrForbidden) {
			return Identity{}, fmt.Errorf("%w", ErrUnauthenticated)
		}
		return Identity{}, err
	}
	if me.Username == "" {
		return Identity{}, ErrNoLoginName
	}
	drive, err := v.c.PersonalDriveID(ctx, cr)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{UserID: me.ID, Username: me.Username, DisplayName: me.DisplayName, DriveID: drive}

	until := now.Add(v.ttl)
	if exp, ok := tokenExpiry(token.Reveal()); ok && exp.Before(until) {
		until = exp
	}
	if until.After(now) {
		v.store(string(key[:]), id, until)
	}
	return id, nil
}

// store caches id until the given time, keeping the cache bounded.
func (v *Validator) store(rawKey string, id Identity, until time.Time) {
	var key [32]byte
	copy(key[:], rawKey)
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
