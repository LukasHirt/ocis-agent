import type { Approval, Settings, Status } from '../api/types'

// Fixtures of the API's JSON shapes for tests.

export const statusOf = (over: Partial<Status> = {}): Status => ({
  enrolled: true,
  user: { id: 'u1', name: 'Albert', login: 'einstein' },
  paused: false,
  pause_reason: '',
  token: { status: 'valid', expires_at: null },
  budget: { used_tokens: 0, daily_tokens: 100000, resets_at: '2026-01-02T00:00:00Z' },
  kill_switch: { database: false, environment: false },
  models: { default: 'm', available: [], preferences: { chat: null, proactive: null } },
  pending_approvals: 0,
  proactive: { enabled: false, last_sweep_at: null, next_sweep_at: null, blocked: 'proactive_disabled', daily_jobs: 24 },
  ...over
})

export const settingsOf = (over: Partial<Settings> = {}): Settings => ({
  proactive_enabled: false,
  watch: [],
  sweep_interval: '15m0s',
  quiet_hours: '',
  daily_tokens: 100000,
  daily_tokens_max: 100000,
  dormancy_days: 30,
  models: { chat: null, proactive: null },
  ...over
})

export const approvalOf = (id: string, over: Partial<Approval> = {}): Approval => ({
  id,
  job_id: 'j1',
  call_id: 'c1',
  tool: 'files.write',
  risk: 'write',
  args: { path: '/a.txt' },
  args_hash: 'h-' + id,
  target_etag: '',
  rationale: '',
  preview: null,
  state: 'pending',
  requested_at: null,
  expires_at: null,
  decided_by: '',
  decided_at: null,
  executed_at: null,
  outcome: null,
  job_ended: false,
  needs_verification: false,
  acknowledged_at: null,
  acknowledged_by: '',
  ...over
})

export const json = (v: unknown, status = 200): Response =>
  new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json' } })

// gatewayErr is what a gateway answers when the sidecar behind it is down: a
// 502 without the sidecar's JSON error body.
export const gatewayErr = (status = 502): Response => new Response('<html>Bad Gateway</html>', { status })

export const apiErr = (status: number, code: string): Response =>
  json({ error: { code, message: 'server text' } }, status)
