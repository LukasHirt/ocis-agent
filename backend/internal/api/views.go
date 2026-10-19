package api

import "time"

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
