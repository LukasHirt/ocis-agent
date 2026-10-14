package store

import (
	"encoding/json"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
)

// Tx is the set of operations available inside a transaction. Methods return
// ErrNotFound, ErrFenceLost or ErrConflict (wrapped or bare) as documented on
// each area's types.
type Tx interface {
	// Now returns the store clock's current time in UTC.
	Now() time.Time

	// GetSystem returns the value for key, or ErrNotFound.
	GetSystem(key string) (string, error)
	// SetSystem creates or replaces the value for key.
	SetSystem(key, value string) error

	// AppendAudit adds an event to the user's chain, assigning the sequence
	// number, previous hash and hash inside the transaction. A zero TS becomes
	// Now(). UserID and Kind are required.
	AppendAudit(ev audit.Event) (audit.Record, error)
	// ReadAudit returns records with seq > afterSeq in ascending order.
	// limit <= 0 means 100; the maximum is 1000.
	ReadAudit(userID string, afterSeq int64, limit int) ([]audit.Record, error)
	// VerifyAudit recomputes the user's whole chain. Failures wrap
	// audit.ErrChainBroken.
	VerifyAudit(userID string) error

	// PutDEK stores a wrapped data-encryption key. ErrConflict if the id
	// exists; a zero CreatedAt becomes Now().
	PutDEK(d DEK) error
	// GetDEK returns the key with the given id, or ErrNotFound.
	GetDEK(id string) (DEK, error)
	// ActiveDEK returns the newest non-retired key, or ErrNotFound if none.
	ActiveDEK() (DEK, error)
	// RetireDEK marks a key as no longer used for new data. Idempotent;
	// ErrNotFound if the id is unknown.
	RetireDEK(id string) error

	// PutUser upserts name and drive id and creates the default policy if the
	// user has none. An existing user keeps EnrolledAt and LastSeenAt.
	PutUser(u User) error
	// GetUser returns the user, or ErrNotFound.
	GetUser(id string) (User, error)
	// TouchUser moves last_seen_at forward (never backwards).
	TouchUser(id string, seenAt time.Time) error
	// DeleteUser removes the user and everything that cascades from it
	// (policy, token, labels, jobs). Audit records are never touched.
	DeleteUser(id string) error

	// GetPolicy returns the user's policy, or ErrNotFound.
	GetPolicy(userID string) (Policy, error)
	// PutPolicy validates and replaces the policy; UpdatedAt becomes Now().
	PutPolicy(p Policy) error

	// PutAppToken inserts or replaces the user's current app token record.
	PutAppToken(t AppToken) error
	// GetAppToken returns the user's token record, or ErrNotFound.
	GetAppToken(userID string) (AppToken, error)
	// SetAppTokenStatus changes the status of the stored token.
	SetAppTokenStatus(userID string, s TokenStatus) error

	// AddTokenLabel records a label this sidecar created. ErrConflict if it
	// already exists.
	AddTokenLabel(userID, label string) error
	// MarkTokenLabelDeleted records that the token with this label was
	// deleted. Idempotent; ErrNotFound if the label is unknown.
	MarkTokenLabelDeleted(userID, label string) error
	// ListTokenLabels returns the user's labels ordered by creation time.
	ListTokenLabels(userID string) ([]TokenLabel, error)

	// InsertJob adds a queued job (attempt 0). NotBefore defaults to Now() and
	// Checkpoint to {}. ErrNotFound if the user does not exist.
	InsertJob(j Job) (Job, error)
	// GetJob returns the user's job, or ErrNotFound.
	GetJob(userID, jobID string) (Job, error)
	// ListJobs returns the user's jobs, newest first. f.Limit <= 0 means 50;
	// the maximum is 500.
	ListJobs(userID string, f JobFilter) ([]Job, error)
	// CancelJob cancels a job that is not terminal yet. ErrNotFound, or
	// ErrConflict if the job is already terminal.
	CancelJob(userID, jobID string) error
	// SaveCheckpoint stores the job's checkpoint. Fenced: ErrFenceLost unless
	// fence is the current lease generation of this running job; ErrConflict
	// if the job is terminal.
	SaveCheckpoint(userID, jobID string, checkpoint json.RawMessage, fence int64) error
	// AppendStep adds a step to a running job's timeline and assigns its N.
	// Fenced like SaveCheckpoint.
	AppendStep(step JobStep, fence int64) (JobStep, error)
	// ListSteps returns the job's steps in order (empty for another user's job).
	ListSteps(userID, jobID string) ([]JobStep, error)

	// ClaimNextJob claims the oldest runnable job whose user has no active
	// lease. The bool is false when nothing is runnable.
	ClaimNextJob(worker string, ttl time.Duration) (Claim, bool, error)
	// RenewLease extends the lease. ErrFenceLost if it is no longer ours.
	RenewLease(c Claim, ttl time.Duration) (Claim, error)
	// ReleaseJob frees the lease and moves the job to next (not running) if it
	// is still running. applied is false if the job was no longer running
	// (for example cancelled). An empty checkpoint keeps the stored one.
	// Attempt counts runs that did not end with a voluntary release, so pauses
	// for approval do not count towards the reaper's max-attempts budget.
	ReleaseJob(c Claim, next JobState, checkpoint json.RawMessage, errMsg string) (applied bool, err error)
	// ReapExpired requeues or fails running jobs whose lease is gone.
	ReapExpired(maxAttempts int, backoff func(attempt int) time.Duration) (ReapResult, error)
}
