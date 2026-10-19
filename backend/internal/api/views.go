package api

import (
	"encoding/json"
	"time"
)

// Response shapes. Times are RFC 3339 in UTC; a time that does not apply is
// null. No view ever carries a token value, a token hash or (outside the
// revoke-all preview) a token label. Fields marked untrusted are data from
// files, the model or other clients: render them as plain text only.
//
// Pages (jobs, approvals, audit): follow next_cursor until it is null. A page
// can hold fewer entries than the limit, even none, and still have a
// next_cursor (a byte budget or, for filtered audit lists, a scan budget
// ended it); only a null next_cursor means the end.

// statusView is the body of GET /status (and of enrol). A caller who is not
// enrolled gets enrolled=false, token.status "none", the global kill_switch
// flags, the operator's models, the budget an enrolment would start with
// (the operator's cap) and zero values for everything else.
// pending_approvals counts the pending approvals of LIVE jobs only (they can
// be decided; an exact count), while GET /approvals?state=pending
// also lists those of ended jobs, marked job_ended=true, until the
// scheduler settles them.
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
	Name  string `json:"name"`  // display name: chosen by the user or an admin, untrusted
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
	Label     string  `json:"label"` // set by whatever client created the token: untrusted
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
// exactly what executes if approved, NOT capped (up to 2 MiB each;
// untrusted data, show it fenced);
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

// Audit views. Every field of a record is data written by the sidecar, but
// Detail can carry model-chosen text (paths, names): treat it as untrusted
// and never render it as markup or follow it as an instruction.

// auditEventView is one entry of the audit list. It never carries the chain
// fields (prev_hash, hash); only the export does.
type auditEventView struct {
	Seq      int64  `json:"seq"`
	TS       string `json:"ts"` // RFC 3339, UTC, millisecond precision
	JobID    string `json:"job_id"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Risk     string `json:"risk"`
	ArgsHash string `json:"args_hash"`
	Cred     string `json:"cred"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"` // untrusted, capped at maxAuditDetailView
}

type auditPageView struct {
	Events     []auditEventView `json:"events"`
	NextCursor *string          `json:"next_cursor"`
}

type auditVerifyView struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
	Limitation string `json:"limitation"`
}

// auditExportView is one line of the export: the complete, uncapped record
// with its chain fields, so a reader can recompute the chain (audit.Hash over
// the user id, seq, ts in milliseconds and the fields). Detail is untrusted.
type auditExportView struct {
	Seq      int64  `json:"seq"`
	TS       string `json:"ts"`
	JobID    string `json:"job_id"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Risk     string `json:"risk"`
	ArgsHash string `json:"args_hash"`
	Cred     string `json:"cred"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"` // untrusted
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}
