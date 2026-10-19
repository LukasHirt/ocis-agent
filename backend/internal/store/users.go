package store

import (
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

// Task kinds a user can prefer a model for.
const (
	ModelTaskChat      = "chat"
	ModelTaskProactive = "proactive"
)

// TriggerChat is the trigger kind of an interactive chat job.
const TriggerChat = "chat"

// ModelTaskFor maps a job's trigger kind to the task kind that selects the
// model: interactive chat jobs use the chat preference, everything else
// (file events, scheduled sweeps) the proactive one.
func ModelTaskFor(triggerKind string) string {
	if triggerKind == TriggerChat {
		return ModelTaskChat
	}
	return ModelTaskProactive
}

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
	DailyTokens      int64 // 0: the operator's cap (DailyBudget); a new user starts there
	DormancyDays     int   // 0 disables the dormancy pause
	// Models maps a task kind (ModelTaskChat, ModelTaskProactive) to the model
	// id the user prefers. The store does not know the operator's catalog:
	// check llm.Catalog.Has before setting one and use llm.Catalog.Resolve
	// before using one. Remove the key to go back to the default.
	Models    map[string]string
	UpdatedAt time.Time
}

// DailyBudget is a user's daily token budget under the operator's cap
// (AGENT_USER_DAILY_TOKENS): a stored value of 0 (or below) or above the cap
// is the cap. Without a cap (limit 0, which the configuration never allows)
// the stored value applies and 0 means unlimited. The status endpoint and
// the runner's budget check both use it, so the limit shown is the one
// enforced.
func DailyBudget(stored, limit int64) int64 {
	if limit > 0 && (stored <= 0 || stored > limit) {
		return limit
	}
	return stored
}

// DefaultPolicy returns the policy a newly enrolled user starts with. Its
// DailyTokens is 0, the operator's cap, so a user who never changed it
// follows the cap when the operator changes it.
func DefaultPolicy(userID string, now time.Time) Policy {
	return Policy{
		UserID:        userID,
		Watch:         []string{},
		SweepInterval: 6 * time.Hour,
		DailyTokens:   0,
		DormancyDays:  30,
		Models:        map[string]string{},
		UpdatedAt:     now,
	}
}

// Validate checks the invariants the store enforces.
func (p Policy) Validate() error {
	switch {
	case p.UserID == "":
		return Invalid("a policy needs a user id")
	case !ValidPauseReason(p.PauseReason):
		return Invalid("unknown pause reason %q", p.PauseReason)
	case p.Paused && p.PauseReason == "":
		return Invalid("a paused policy needs a pause reason")
	case !p.Paused && p.PauseReason != "":
		return Invalid("a pause reason requires paused=true")
	case p.SweepInterval < time.Second || p.SweepInterval%time.Second != 0:
		return Invalid("the sweep interval must be a whole number of seconds, at least one second")
	case p.DailyTokens < 0:
		return Invalid("daily tokens must not be negative")
	case p.DormancyDays < 0:
		return Invalid("dormancy days must not be negative")
	}
	for task, id := range p.Models {
		if task != ModelTaskChat && task != ModelTaskProactive {
			return Invalid("unknown model task kind")
		}
		if !llm.ValidModelID(id) {
			return Invalid("the preferred model for %s is not a valid model id", task)
		}
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
