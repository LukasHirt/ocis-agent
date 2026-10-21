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
	// AcknowledgedAt and AcknowledgedBy record that the user checked an
	// uncertain outcome (AcknowledgeApproval). They change neither the
	// state nor the result.
	AcknowledgedAt *time.Time
	AcknowledgedBy string
}

// recordedOutcome is the shape of a readable Result: the executor's
// {"outcome","result":{"summary","diff",...},"reason"}.
type recordedOutcome struct {
	Outcome string `json:"outcome"`
	Result  *struct {
		Summary string `json:"summary"`
		Diff    string `json:"diff"`
	} `json:"result"`
	Reason string `json:"reason"`
}

// OutcomeUncertain reports whether nobody knows for sure what the approved
// action did: the state is unknown, or a stored result is unreadable or
// records an unknown outcome. The user should check their files.
func (a Approval) OutcomeUncertain() bool {
	if a.State == ApprovalUnknown {
		return true
	}
	if len(a.Result) == 0 {
		return false
	}
	var o recordedOutcome
	if err := json.Unmarshal(a.Result, &o); err != nil {
		return true
	}
	switch o.Outcome {
	case "done", "failed", "expired":
		return false
	}
	return true
}

// NeedsVerification reports an uncertain outcome the user has not
// acknowledged yet.
func (a Approval) NeedsVerification() bool { return a.AcknowledgedAt == nil && a.OutcomeUncertain() }

// ApprovalFilter narrows ListApprovals.
type ApprovalFilter struct {
	States []ApprovalState
	JobID  string
	Limit  int
	// Before, if set, keeps only the approvals strictly after this key in
	// the list order (requested_at DESC, id DESC): keyset pagination with
	// the RequestedAt and ID of the previous page's last approval.
	Before *PageKey
	// MaxBytes, if > 0, ends the list once the rows' Args and Preview
	// together reach this many bytes (the row that reaches it is included,
	// so at least one row is returned): a list of approvals, whose arguments
	// can be up to 2 MiB each, stays within the budget plus one row. The
	// rows past the budget are never loaded. < 0 is ErrInvalid.
	MaxBytes int64
	// NeedsVerification keeps only the approvals whose NeedsVerification
	// is true (together with the other conditions).
	NeedsVerification bool
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
