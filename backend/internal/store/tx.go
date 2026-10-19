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
	// ListJobSummaries is ListJobs without the checkpoints and with each
	// job's step count, in one query (same order, filter and limits).
	ListJobSummaries(userID string, f JobFilter) ([]JobSummary, error)
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
	// SumJobTokens returns the input and output tokens of all steps (llm and
	// tool) of all of the user's jobs, finished ones included, whose step
	// created_at is at or after since (zero means from the beginning). An
	// unknown user gives 0, 0, nil.
	SumJobTokens(userID string, since time.Time) (in, out int64, err error)
	// ListSteps returns the job's steps in order (empty for another user's job).
	ListSteps(userID, jobID string) ([]JobStep, error)

	// ClaimNextJob claims the oldest runnable job whose user has no active
	// lease and is not paused (a job already running is not touched). While
	// the kill switch (KillSwitchKey) is on it claims nothing. The bool is
	// false when nothing is runnable.
	ClaimNextJob(worker string, ttl time.Duration) (Claim, bool, error)
	// RenewLease extends the lease. ErrFenceLost if it is no longer ours.
	RenewLease(c Claim, ttl time.Duration) (Claim, error)
	// ReleaseJob frees the lease and moves the job to next (not running) if it
	// is still running. applied is false if the job was no longer running
	// (for example cancelled). An empty checkpoint keeps the stored one.
	// A release back to queued or waiting_approval refunds the attempt, so only
	// runs that crashed or ended terminally count towards the reaper's budget.
	ReleaseJob(c Claim, next JobState, checkpoint json.RawMessage, errMsg string) (applied bool, err error)
	// ReapExpired requeues or fails running jobs whose lease is gone.
	ReapExpired(maxAttempts int, backoff func(attempt int) time.Duration) (ReapResult, error)

	// AssertLease succeeds only if the job is running and the user's lease
	// names this job with this fence and a non-blank holder. Use it inside a
	// composite transaction (approval transition + idempotency + audit + step)
	// to prove the caller still owns the job. ErrNotFound if there is no such
	// job, ErrConflict if it is terminal, ErrFenceLost otherwise.
	AssertLease(userID, jobID string, fence int64) error
	// RequeueJob releases the claim back to queued (as ReleaseJob does) and
	// delays the next claim until notBefore (zero means now). applied is false
	// if the job was no longer running.
	RequeueJob(c Claim, notBefore time.Time, checkpoint json.RawMessage) (applied bool, err error)

	// InsertApproval adds a pending approval for a running job. Fenced: the
	// caller must hold the job's lease (ErrFenceLost / ErrConflict /
	// ErrNotFound as for AssertLease). Invalid input gives ErrInvalid; a
	// duplicate id or idempotency key gives ErrConflict. It must run in the
	// SAME transaction as ReleaseJob(waiting_approval), so a job never waits
	// without its approvals, nor has approvals while still running.
	InsertApproval(a Approval, fence int64) (Approval, error)
	// GetApproval returns the user's approval, or ErrNotFound.
	GetApproval(userID, id string) (Approval, error)
	// ListApprovals returns the user's approvals, newest first. f.Limit <= 0
	// means 50; the maximum is 500.
	ListApprovals(userID string, f ApprovalFilter) ([]Approval, error)
	// DecideApproval records the user's decision (approved or rejected) on a
	// pending approval. ErrNotFound; ErrConflict if it is no longer pending;
	// ErrExpired if it is past ExpiresAt (state unchanged); ErrInvalid if the
	// decision is not approved/rejected, decidedBy is empty or argsHash does
	// not match the stored hash (state unchanged). Run it in the same
	// transaction as the approval_decided audit event and ResumeJobIfSettled,
	// so a decision is never recorded without the job being resumed.
	DecideApproval(userID, id string, decision ApprovalState, decidedBy, argsHash string) (Approval, error)
	// TransitionApproval is the worker's compare-and-swap between states, for
	// the edges ApprovalTransitionAllowed permits. ErrInvalid for any other
	// edge, ErrNotFound, ErrConflict if the current state is not from.
	TransitionApproval(userID, id string, from, to ApprovalState, patch ApprovalPatch) (Approval, error)
	// ExpireApprovals moves every pending approval past its ExpiresAt to
	// expired and returns what it changed. Run it in the same transaction as
	// ResumeJobIfSettled for every returned ref: the refs are never returned
	// again, so a split would strand the job in waiting_approval.
	ExpireApprovals() ([]ApprovalRef, error)
	// ExpireEndedJobApprovals moves the pending and approved approvals of
	// jobs that have ended (succeeded, failed, cancelled) to expired, oldest
	// first (requested_at, then id), at most limit rows (limit <= 0 means
	// 100, the maximum is 1000), and returns what it changed. An approved
	// row keeps its decided_by and decided_at. It never touches executing
	// approvals (their outcome is open and is reconciled instead), settled
	// ones, or the approvals of a job that has not ended. Run it in the same
	// transaction as the approval_expired audit event for every returned ref.
	ExpireEndedJobApprovals(limit int) ([]ApprovalRef, error)
	// ResumeJobIfSettled moves a job from waiting_approval back to queued
	// (claimable now) when none of its approvals is still pending. It returns
	// false if the job is not waiting or something is still pending. Call it
	// in the same transaction as the DecideApproval or ExpireApprovals that may
	// have settled the job (see those methods).
	ResumeJobIfSettled(userID, jobID string) (resumed bool, err error)

	// BeginIdempotent records that a call with this key has started. If the
	// key is new it returns nil. If the key exists nothing changes and the
	// existing record is returned, so a restarted worker sees started, done or
	// failed instead of running the call again. Key, UserID and Tool are
	// required (ErrInvalid); a key owned by another user is ErrConflict; an
	// unknown user is ErrNotFound. In transaction A of the execution protocol
	// an ErrConflict from the preceding approved->executing transition means
	// "already started: reconcile via GetIdempotent". Keys must be derived by
	// the server (job id, call id, arguments hash), never taken from user input.
	BeginIdempotent(rec IdemRecord) (existing *IdemRecord, err error)
	// FinishIdempotent moves a started record to done or failed and stores the
	// result. ErrInvalid for other states or malformed JSON, ErrNotFound, and
	// ErrConflict if the record is not started.
	FinishIdempotent(userID, key string, state IdemState, result json.RawMessage) error
	// GetIdempotent returns the user's record, or ErrNotFound.
	GetIdempotent(userID, key string) (IdemRecord, error)
	// ListStartedIdempotent returns started records with started_at <=
	// olderThan, oldest first (calls whose outcome may be unknown). limit <= 0
	// means 100; the maximum is 1000. No production code calls it (the
	// scheduler reconciles via ListStartedIdempotentOfEndedJobs); it serves
	// diagnostics and tests.
	ListStartedIdempotent(olderThan time.Time, limit int) ([]IdemRecord, error)
	// ListStartedIdempotentOfEndedJobs returns the same records as
	// ListStartedIdempotent, but only those whose job (the key's prefix before
	// the first ":", a job of the record's user) has ended (succeeded, failed,
	// cancelled) or no longer exists, oldest first. Records of active jobs are
	// left out, so they cannot hide the records of ended jobs behind them.
	// limit <= 0 means 100; the maximum is 1000.
	ListStartedIdempotentOfEndedJobs(olderThan time.Time, limit int) ([]IdemRecord, error)

	// PausePolicy pauses the agent for reason unless a pause that outranks it
	// is already in place (see PauseSupersedes). changed is false if nothing
	// was written. ErrInvalid for an empty or unknown reason, ErrNotFound.
	PausePolicy(userID, reason string) (changed bool, err error)
	// ClearPause resumes the agent only if the current pause reason is one of
	// reasons. ErrInvalid if none or an unknown reason is given; ErrNotFound.
	ClearPause(userID string, reasons ...string) (cleared bool, err error)
	// WipeAppToken empties the stored ciphertext and nonce and marks the token
	// revoked (the label and key reference stay). ErrNotFound if there is no
	// token. Sealed data is encrypted, so this is a removal of the secret from
	// the live row, not a secure erase of free pages.
	WipeAppToken(userID string) error

	// ListUsers returns users in user id order after afterID. limit <= 0 means
	// 100; the maximum is 1000.
	ListUsers(afterID string, limit int) ([]User, error)
	// ListDormantUsers returns the ids of users that are not paused, have
	// dormancy_days > 0 and were last seen at or before now minus that many
	// days, least recently seen first. limit <= 0 means 100; the maximum is
	// 1000.
	ListDormantUsers(now time.Time, limit int) ([]string, error)
	// ListTokensDueForRenewal returns valid tokens that have reached half of
	// their lifetime (including expired ones), soonest expiry first. limit <= 0
	// means 100; the maximum is 1000.
	ListTokensDueForRenewal(now time.Time, limit int) ([]TokenDue, error)

	// GetCursor returns the user's cursor; an absent cursor is a zero cursor
	// with an empty, non-nil MemoryETags map.
	GetCursor(userID string) (Cursor, error)
	// PutCursor inserts or replaces the user's cursor. ErrInvalid without a
	// user id, ErrNotFound for an unknown user.
	PutCursor(c Cursor) error
}
