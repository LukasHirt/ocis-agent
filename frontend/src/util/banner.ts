import type { StreamState } from '../api/events'
import type { PauseReason, Status } from '../api/types'

// The status banner shows the single most severe state, in this order.
export type BannerKind =
  | 'killswitch'
  | 'reconnect'
  | 'token_anomaly'
  | 'paused'
  | 'user'
  | 'dormant'
  | 'budget'

const CREDENTIAL_REASONS = new Set<Status['pause_reason']>([
  'credential_invalid',
  'credential_revoked',
  'credential_expired'
] satisfies PauseReason[])

export const budgetUsedUp = (s: Status): boolean => {
  const b = s.budget
  return !!b && b.daily_tokens > 0 && b.used_tokens >= b.daily_tokens
}

export const bannerOf = (s: Status | null): BannerKind | null => {
  if (!s) return null
  if (s.kill_switch?.database || s.kill_switch?.environment) return 'killswitch'
  if (s.paused) {
    const r = s.pause_reason
    if (CREDENTIAL_REASONS.has(r)) return 'reconnect'
    if (r === ('token_anomaly' satisfies PauseReason)) return 'token_anomaly'
    if (r === ('user' satisfies PauseReason)) return 'user'
    if (r === ('dormant' satisfies PauseReason)) return 'dormant'
    return 'paused'
  }
  if (budgetUsedUp(s)) return 'budget'
  return null
}

export const connectionNoteOf = (c: StreamState): 'polling' | 'offline' | null =>
  c === 'polling' || c === 'offline' ? c : null

// utcTime renders the time of an ISO timestamp as HH:MM in UTC ('' if invalid).
export const utcTime = (iso: string | null): string => {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`
}
