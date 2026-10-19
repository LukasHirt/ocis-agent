// JSON shapes of the sidecar's REST API (/agent-api/v1), field for field as
// the server encodes them. Times are RFC 3339 strings in UTC, or null when
// they do not apply. Fields documented as untrusted are file data, model
// output or other clients' text: render them as plain text only. The server
// may add fields; unknown fields are ignored.

// A page of a list: follow next_cursor until it is null. A page can hold
// fewer entries than the limit, even none, and still carry a cursor.
export type Page<K extends string, T> = { [P in K]: T[] } & { next_cursor: string | null }

export type PauseReason =
  | ''
  | 'user'
  | 'dormant'
  | 'token_anomaly'
  | 'credential_invalid'
  | 'credential_revoked'
  | 'credential_expired'

export type TokenStatus = 'valid' | 'invalid' | 'revoked' | 'none'

export interface StatusUser {
  id: string
  name: string // display name: untrusted
  login: string
}

export interface StatusToken {
  status: TokenStatus | string
  expires_at: string | null
}

export interface Budget {
  used_tokens: number // today (UTC)
  daily_tokens: number // never above the operator cap
  resets_at: string | null // the next UTC midnight
}

export interface KillSwitch {
  database: boolean
  environment: boolean
}

export interface Model {
  id: string
  label: string
}

export interface ModelPreferences {
  chat: string | null
  proactive: string | null
}

export interface Models {
  default: string
  available: Model[]
  preferences: ModelPreferences
}

// GET /status (and the body of enrol, pause and resume).
export interface Status {
  enrolled: boolean
  user: StatusUser
  paused: boolean
  pause_reason: PauseReason | string
  token: StatusToken
  budget: Budget
  kill_switch: KillSwitch
  models: Models
  pending_approvals: number // pending approvals of live jobs only
}

export interface Revoked {
  revoked: number
}

export interface Renewed {
  renewed: boolean
  old_cleaned: boolean
}

// One of the user's app tokens in the revoke-all confirmation.
export interface TokenSummary {
  label: string // untrusted
  created_at: string | null
  expires_at: string | null
  ours: boolean
}

export interface RevokeAllPreview {
  tokens: TokenSummary[]
}

// GET and PUT /settings.
export interface Settings {
  proactive_enabled: boolean
  watch: string[] // the user's own paths: untrusted text
  sweep_interval: string // a Go duration, e.g. "15m0s"
  quiet_hours: string
  daily_tokens: number
  daily_tokens_max: number // the operator cap, read-only
  dormancy_days: number // read-only
  models: ModelPreferences
}

// PUT /settings body: only the mentioned fields change (at least one).
// daily_tokens null follows the operator cap again; a model null clears
// the preference of that task.
export interface SettingsUpdate {
  proactive_enabled?: boolean
  watch?: string[]
  sweep_interval?: string
  quiet_hours?: string
  daily_tokens?: number | null
  models?: { chat?: string | null; proactive?: string | null }
}

export type JobState = 'queued' | 'running' | 'waiting_approval' | 'succeeded' | 'failed' | 'cancelled'

export interface Job {
  id: string
  trigger_kind: string
  goal: string // untrusted, capped
  state: JobState | string
  attempt: number
  runs: number
  created_at: string | null
  updated_at: string | null
  error: string // a fixed reason text, safe to show
  steps_count: number
}

export interface Step {
  n: number
  kind: string
  tool: string
  call_id: string
  status: string
  tokens_in: number
  tokens_out: number
  summary: string // untrusted, capped
  approval_id: string
  created_at: string | null
}

export interface JobDetail extends Job {
  steps: Step[]
  answer: string // model text: untrusted, capped
}

export type JobPage = Page<'jobs', Job>

export type ApprovalState =
  | 'pending'
  | 'approved'
  | 'rejected'
  | 'expired'
  | 'executing'
  | 'executed'
  | 'failed'
  | 'unknown'

export type Risk = 'read' | 'write' | 'destructive' | 'outward'

export interface Outcome {
  state: string // done, failed, unknown, rejected or expired
  summary: string // untrusted, capped
  reason: string
  diff: string // untrusted, capped
}

export interface Approval {
  id: string
  job_id: string
  call_id: string
  tool: string
  risk: Risk | string
  args: unknown // the stored JSON, exactly what executes: untrusted
  args_hash: string
  target_etag: string
  rationale: string // the model's explanation: untrusted, unverified
  preview: unknown // untrusted
  state: ApprovalState | string
  requested_at: string | null
  expires_at: string | null
  decided_by: string
  decided_at: string | null
  executed_at: string | null
  outcome: Outcome | null
  job_ended: boolean
  needs_verification: boolean
}

export type ApprovalPage = Page<'approvals', Approval>

export interface Approvals {
  approvals: Approval[]
}

export type Decision = 'approve' | 'reject'

export interface DecisionItem {
  id: string
  args_hash: string
}

export interface Conversation {
  id: string
  title: string // untrusted
  created_at: string | null
  updated_at: string | null
}

export type ConversationPage = Page<'conversations', Conversation>

export interface MessageJob {
  id: string
  state: JobState | string
  error: string
}

export interface Message {
  id: string
  role: 'user' | 'assistant' | 'system' | string
  content: string // untrusted, capped
  job_id: string
  created_at: string | null
  job: MessageJob | null
}

export type MessagePage = Page<'messages', Message>

export interface Sent {
  message: Message
  job: { id: string; state: JobState | string }
  agent: { paused: boolean; pause_reason: PauseReason | string; kill_switch: boolean }
}

export interface MemoryFileInfo {
  name: string
  size: number
  etag: string
  modified: string
}

export interface MemoryList {
  files: MemoryFileInfo[]
}

export interface MemoryFile {
  name: string
  content: string // untrusted
  etag: string
  truncated: boolean
  editable: boolean
}

export interface MemoryEdit {
  name: string
  etag: string
  diff: string // untrusted, capped
}

export interface AuditEvent {
  seq: number
  ts: string
  job_id: string
  kind: string
  tool: string
  risk: string
  args_hash: string
  cred: string
  outcome: string
  detail: string // untrusted, capped
}

export type AuditPage = Page<'events', AuditEvent>

export interface AuditVerify {
  ok: boolean
  message?: string
  limitation: string
}

export interface ApiErrorBody {
  error: { code: string; message: string; request_id?: string }
}
