package store

import (
	"errors"
	"fmt"
	"time"
)

// User is an enrolled user.
type User struct {
	ID              string
	Name            string
	PersonalDriveID string
	EnrolledAt      time.Time
	LastSeenAt      time.Time
}

// Pause reasons. An empty reason means the agent is not paused.
const (
	PauseUser              = "user"
	PauseDormant           = "dormant"
	PauseTokenAnomaly      = "token_anomaly"
	PauseCredentialInvalid = "credential_invalid"
	PauseCredentialRevoked = "credential_revoked"
	PauseCredentialExpired = "credential_expired"
)

// ValidPauseReason reports whether r is a known pause reason (or empty).
func ValidPauseReason(r string) bool {
	switch r {
	case "", PauseUser, PauseDormant, PauseTokenAnomaly, PauseCredentialInvalid, PauseCredentialRevoked, PauseCredentialExpired:
		return true
	}
	return false
}

// Policy is a user's authoritative policy and preferences. It lives only in
// the database; nothing the model reads can change it.
type Policy struct {
	UserID           string
	Paused           bool
	PauseReason      string
	AutoApproveWrite bool // reserved: not exposed in v1
	ProactiveEnabled bool
	Watch            []string
	SweepInterval    time.Duration
	QuietHours       string
	DailyTokens      int64
	DormancyDays     int // 0 disables the dormancy pause
	UpdatedAt        time.Time
}

// DefaultPolicy returns the policy a newly enrolled user starts with.
func DefaultPolicy(userID string, now time.Time) Policy {
	return Policy{
		UserID:        userID,
		Watch:         []string{},
		SweepInterval: 6 * time.Hour,
		DailyTokens:   200000,
		DormancyDays:  30,
		UpdatedAt:     now,
	}
}

// Validate checks the invariants the store enforces.
func (p Policy) Validate() error {
	switch {
	case p.UserID == "":
		return errors.New("store: a policy needs a user id")
	case !ValidPauseReason(p.PauseReason):
		return fmt.Errorf("store: unknown pause reason %q", p.PauseReason)
	case p.Paused && p.PauseReason == "":
		return errors.New("store: a paused policy needs a pause reason")
	case !p.Paused && p.PauseReason != "":
		return errors.New("store: a pause reason requires paused=true")
	case p.SweepInterval < time.Second || p.SweepInterval%time.Second != 0:
		return errors.New("store: the sweep interval must be a whole number of seconds, at least one second")
	case p.DailyTokens < 0:
		return errors.New("store: daily tokens must not be negative")
	case p.DormancyDays < 0:
		return errors.New("store: dormancy days must not be negative")
	}
	return nil
}

// TokenStatus is the state of the stored app token.
type TokenStatus string

// Token statuses.
const (
	TokenValid   TokenStatus = "valid"
	TokenInvalid TokenStatus = "invalid"
	TokenRevoked TokenStatus = "revoked"
)

// Valid reports whether s is a known status.
func (s TokenStatus) Valid() bool {
	return s == TokenValid || s == TokenInvalid || s == TokenRevoked
}

// AppToken is the user's current app token, encrypted. The value never
// appears here in plaintext.
type AppToken struct {
	UserID    string
	Sealed    Sealed
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
	Status    TokenStatus
}

// TokenLabel records a label this sidecar gave to an app token it created
// (labels only, never token values or hashes). The unknown-token monitor
// compares oCIS's token list against these.
type TokenLabel struct {
	UserID    string
	Label     string
	CreatedAt time.Time
	DeletedAt *time.Time
}
