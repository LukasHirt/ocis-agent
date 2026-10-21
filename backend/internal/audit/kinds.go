package audit

// Event kinds the agent records. Every emitter uses these constants, and the
// API accepts exactly this set as a filter.
const (
	KindActionExecuted      = "action_executed"
	KindAgentDormant        = "agent_dormant"
	KindAgentPaused         = "agent_paused"
	KindAgentResumed        = "agent_resumed"
	KindApprovalDecided     = "approval_decided"
	KindApprovalExpired     = "approval_expired"
	KindApprovalRequested   = "approval_requested"
	KindChatMessage         = "chat_message"
	KindConversationDeleted = "conversation_deleted"
	KindJobCancelled        = "job_cancelled"
	KindJobFinished         = "job_finished"
	KindMemoryChanged       = "memory_changed"
	KindMemoryEdited        = "memory_edited"
	KindPolicyChanged       = "policy_changed"
	KindPolicyDenied        = "policy_denied"
	KindTampered            = "tampered"
	KindTokenAnomaly        = "token_anomaly"
	KindTokenCreated        = "token_created"
	KindTokenDeleted        = "token_deleted"
	KindTokenExpired        = "token_expired"
	KindTokenInvalid        = "token_invalid"
	KindTokenLabelClosed    = "token_label_closed"
	KindTokenRenewed        = "token_renewed"
	KindTokenRevokedAll     = "token_revoked_all"
	KindToolCall            = "tool_call"
	KindUnenrolled          = "unenrolled"
)

var kinds = map[string]bool{
	KindActionExecuted: true, KindAgentDormant: true, KindAgentPaused: true, KindAgentResumed: true,
	KindApprovalDecided: true, KindApprovalExpired: true, KindApprovalRequested: true, KindChatMessage: true,
	KindConversationDeleted: true, KindJobCancelled: true, KindJobFinished: true, KindMemoryChanged: true,
	KindMemoryEdited: true, KindPolicyChanged: true, KindPolicyDenied: true, KindTampered: true,
	KindTokenAnomaly: true, KindTokenCreated: true, KindTokenDeleted: true, KindTokenExpired: true,
	KindTokenInvalid: true, KindTokenLabelClosed: true, KindTokenRenewed: true, KindTokenRevokedAll: true,
	KindToolCall: true, KindUnenrolled: true,
}

// IsKind reports whether k is one of the kinds the agent records.
func IsKind(k string) bool { return kinds[k] }
