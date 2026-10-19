package api

import (
	"encoding/json"
	"time"
)

// Response shapes. Times are RFC 3339 in UTC; a time that does not apply is
// null. No view ever carries a token value, a token hash or (outside the
// revoke-all preview) a token label.

// statusView is the body of GET /status (and of enrol). A caller who is not
// enrolled gets enrolled=false and zero values everywhere else, except
// token.status "none".
type statusView struct {
	Enrolled         bool           `json:"enrolled"`
	User             userView       `json:"user"`
	Paused           bool           `json:"paused"`
	PauseReason      string         `json:"pause_reason"` // "" or a store.Pause* reason
	Token            tokenView      `json:"token"`
	Budget           budgetView     `json:"budget"`
	KillSwitch       killSwitchView `json:"kill_switch"`
	Models           modelsView     `json:"models"`
	PendingApprovals int            `json:"pending_approvals"`
}

type userView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`  // display name
	Login string `json:"login"` // login name
}

type tokenView struct {
	Status    string  `json:"status"` // valid, invalid, revoked or none
	ExpiresAt *string `json:"expires_at"`
}

type budgetView struct {
	UsedTokens  int64   `json:"used_tokens"`  // today (UTC)
	DailyTokens int64   `json:"daily_tokens"` // the user's budget, never above the operator cap
	ResetsAt    *string `json:"resets_at"`    // the next UTC midnight
}

type killSwitchView struct {
	Database    bool `json:"database"`    // the database flag (ocis-agent killswitch)
	Environment bool `json:"environment"` // AGENT_KILL_SWITCH
}

type modelsView struct {
	Default     string          `json:"default"`
	Available   []modelView     `json:"available"`
	Preferences preferencesView `json:"preferences"`
}

type modelView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// preferencesView shows a stored preference only while it is in the
// catalog (otherwise null: the runner would not use it).
type preferencesView struct {
	Chat      *string `json:"chat"`
	Proactive *string `json:"proactive"`
}

type revokedView struct {
	Revoked int `json:"revoked"`
}

type renewedView struct {
	Renewed    bool `json:"renewed"`
	OldCleaned bool `json:"old_cleaned"`
}

type previewView struct {
	Tokens []tokenSummaryView `json:"tokens"`
}

// tokenSummaryView is one of the user's app tokens in the revoke-all
// confirmation: the label (the user's own app password names) and dates,
// never a hash or a value.
type tokenSummaryView struct {
	Label     string  `json:"label"`
	CreatedAt *string `json:"created_at"`
	ExpiresAt *string `json:"expires_at"`
	Ours      bool    `json:"ours"`
}

// timeView formats t as RFC 3339 in UTC; the zero time is null.
func timeView(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// timePtrView is timeView of an optional time.
func timePtrView(t *time.Time) *string {
	if t == nil {
		return nil
	}
	return timeView(*t)
}

// settingsView is the body of GET and PUT /settings: the user's editable
// policy fields plus the read-only operator cap and dormancy window.
type settingsView struct {
	ProactiveEnabled bool           `json:"proactive_enabled"`
	Watch            []string       `json:"watch"` // the user's own paths: untrusted text
	SweepInterval    string         `json:"sweep_interval"`
	QuietHours       string         `json:"quiet_hours"`
	DailyTokens      int64          `json:"daily_tokens"`     // never above the operator cap
	DailyTokensMax   int64          `json:"daily_tokens_max"` // the operator cap
	DormancyDays     int            `json:"dormancy_days"`    // read-only
	Models           settingsModels `json:"models"`
}

// settingsModels are the stored model preferences that are still in the
// catalog (null otherwise).
type settingsModels struct {
	Chat      *string `json:"chat"`
	Proactive *string `json:"proactive"`
}

// jobView is a job in GET /jobs and the body of a cancel. Goal is the
// user's own text for chat jobs and a server-made instruction for proactive
// ones (which must never embed file names); it is shown as untrusted text
// either way and capped at maxGoalView. Error is the runner's fixed reason
// text (agent.Reason*), safe to show.
type jobView struct {
	ID          string  `json:"id"`
	TriggerKind string  `json:"trigger_kind"`
	Goal        string  `json:"goal"` // untrusted, capped
	State       string  `json:"state"`
	Attempt     int     `json:"attempt"`
	Runs        int     `json:"runs"`
	CreatedAt   *string `json:"created_at"`
	UpdatedAt   *string `json:"updated_at"`
	Error       string  `json:"error"`
	StepsCount  int     `json:"steps_count"`
}

// jobDetailView is GET /jobs/{id}: the job, its timeline and the final
// answer (model text: untrusted, capped at maxAnswerView; empty while there
// is none or the checkpoint cannot be read).
type jobDetailView struct {
	jobView
	Steps  []stepView `json:"steps"`
	Answer string     `json:"answer"`
}

// stepView is one timeline entry. Summary is tool-made text that can echo
// file data or model-supplied paths: untrusted, capped at maxSummaryView.
type stepView struct {
	N          int     `json:"n"`
	Kind       string  `json:"kind"`
	Tool       string  `json:"tool"`
	CallID     string  `json:"call_id"`
	Status     string  `json:"status"`
	TokensIn   int64   `json:"tokens_in"`
	TokensOut  int64   `json:"tokens_out"`
	Summary    string  `json:"summary"` // untrusted, capped
	ApprovalID string  `json:"approval_id"`
	CreatedAt  *string `json:"created_at"`
}

// jobPageView is a page of jobView entries (encoded one by one for the
// byte budget).
type jobPageView struct {
	Jobs       []json.RawMessage `json:"jobs"`
	NextCursor *string           `json:"next_cursor"`
}

// approvalView is one approval. Args and Preview are the stored JSON,
// exactly what executes if approved (untrusted data, show it fenced);
// Rationale is the model's explanation (untrusted, unverified, capped).
// JobEnded marks a pending approval whose job has already ended: it can no
// longer be decided and the scheduler settles it soon. NeedsVerification
// marks an action whose outcome is unknown: the user should check it.
type approvalView struct {
	ID                string          `json:"id"`
	JobID             string          `json:"job_id"`
	CallID            string          `json:"call_id"`
	Tool              string          `json:"tool"`
	Risk              string          `json:"risk"`
	Args              json.RawMessage `json:"args"` // untrusted
	ArgsHash          string          `json:"args_hash"`
	TargetETag        string          `json:"target_etag"`
	Rationale         string          `json:"rationale"` // untrusted, capped
	Preview           json.RawMessage `json:"preview"`   // untrusted
	State             string          `json:"state"`
	RequestedAt       *string         `json:"requested_at"`
	ExpiresAt         *string         `json:"expires_at"`
	DecidedBy         string          `json:"decided_by"`
	DecidedAt         *string         `json:"decided_at"`
	ExecutedAt        *string         `json:"executed_at"`
	Outcome           *outcomeView    `json:"outcome"` // null while undecided or not yet run
	JobEnded          bool            `json:"job_ended"`
	NeedsVerification bool            `json:"needs_verification"`
}

// outcomeView is how a decided approval ended. State is done, failed,
// unknown or expired (from the executor's record) or rejected/expired for
// settled approvals without one. Summary and Diff are tool output that can
// carry file data: untrusted, capped. Reason is the executor's or a fixed
// text, model-safe (it may echo a path the model supplied).
type outcomeView struct {
	State   string `json:"state"`
	Summary string `json:"summary"` // untrusted, capped
	Reason  string `json:"reason"`
	Diff    string `json:"diff"` // untrusted, capped
}

// approvalPageView is a page of approvalView entries.
type approvalPageView struct {
	Approvals  []json.RawMessage `json:"approvals"`
	NextCursor *string           `json:"next_cursor"`
}

type approvalsView struct {
	Approvals []approvalView `json:"approvals"`
}
