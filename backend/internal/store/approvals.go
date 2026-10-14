package store

import (
	"encoding/json"
	"time"
)

// ApprovalState is the state of an approval request.
type ApprovalState string

// Approval states.
const (
	ApprovalPending   ApprovalState = "pending"
	ApprovalApproved  ApprovalState = "approved"
	ApprovalRejected  ApprovalState = "rejected"
	ApprovalExpired   ApprovalState = "expired"
	ApprovalExecuting ApprovalState = "executing"
	ApprovalExecuted  ApprovalState = "executed"
	ApprovalFailed    ApprovalState = "failed"
	ApprovalUnknown   ApprovalState = "unknown"
)

// Risk levels an approval can carry. Reads never need an approval.
const (
	RiskWrite       = "write"
	RiskDestructive = "destructive"
	RiskOutward     = "outward"
)

// Valid reports whether s is a known state.
func (s ApprovalState) Valid() bool {
	switch s {
	case ApprovalPending, ApprovalApproved, ApprovalRejected, ApprovalExpired,
		ApprovalExecuting, ApprovalExecuted, ApprovalFailed, ApprovalUnknown:
		return true
	}
	return false
}

// Terminal reports whether no further transition is possible. An approval in
// state unknown is terminal: the user decides whether to retry, which creates
// a new approval.
func (s ApprovalState) Terminal() bool {
	switch s {
	case ApprovalRejected, ApprovalExpired, ApprovalExecuted, ApprovalFailed, ApprovalUnknown:
		return true
	}
	return false
}

// ApprovalTransitionAllowed reports whether TransitionApproval may move an
// approval from one state to another. Leaving pending is not covered here: it
// is done only by DecideApproval (a user decision) and ExpireApprovals.
func ApprovalTransitionAllowed(from, to ApprovalState) bool {
	switch from {
	case ApprovalApproved:
		return to == ApprovalExecuting || to == ApprovalExpired || to == ApprovalFailed
	case ApprovalExecuting:
		return to == ApprovalExecuted || to == ApprovalFailed || to == ApprovalUnknown
	}
	return false
}

// Approval is a request for the user's consent to one tool call. Args are the
// exact arguments to execute; ArgsHash binds the user's decision to them.
type Approval struct {
	ID          string
	UserID      string
	JobID       string
	CallID      string
	Tool        string
	Risk        string
	Args        json.RawMessage
	ArgsHash    string
	TargetETag  string
	Rationale   string
	Preview     json.RawMessage
	IdemKey     string
	State       ApprovalState
	RequestedAt time.Time
	ExpiresAt   time.Time
	DecidedBy   string
	DecidedAt   *time.Time
	ExecutedAt  *time.Time
	Result      json.RawMessage
}

// ApprovalFilter narrows ListApprovals.
type ApprovalFilter struct {
	States []ApprovalState
	JobID  string
	Limit  int
}

// ApprovalPatch carries what a worker transition may record.
type ApprovalPatch struct {
	Result json.RawMessage
}

// ApprovalRef identifies an approval and the job waiting on it.
type ApprovalRef struct {
	ID     string
	UserID string
	JobID  string
}
