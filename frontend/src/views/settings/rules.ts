import type { ModelPreferences, Settings, SettingsUpdate } from '../../api/types'

// The settings form: a draft of the editable fields, the client-side checks
// (the same rules as the server's, which stays authoritative) and the
// update that carries only what the user changed.

export const MAX_WATCH = 20
export const MAX_WATCH_PATH_BYTES = 1024
export const MIN_DAILY_TOKENS = 1000
// The agent's own folder in the personal space, never watched.
const RESERVED_ROOT = '/.ocis-agent'

export type WatchPathProblem = 'empty' | 'relative' | 'too_long' | 'control' | 'dot_segment' | 'reserved'

// C0 and C1 controls, line and paragraph separators and bidirectional
// controls (zero-width joiners stay allowed), like the server.
const isControl = (cp: number): boolean =>
  cp < 0x20 ||
  (cp >= 0x7f && cp <= 0x9f) ||
  cp === 0x2028 ||
  cp === 0x2029 ||
  cp === 0x200e ||
  cp === 0x200f ||
  cp === 0x061c ||
  (cp >= 0x202a && cp <= 0x202e) ||
  (cp >= 0x2066 && cp <= 0x2069)

const utf8Length = (s: string): number => new TextEncoder().encode(s).length

// cleanWatchPath collapses repeated slashes and drops a trailing one (the
// path must already be free of . and .. segments).
export const cleanWatchPath = (p: string): string => {
  const c = p.replace(/\/+/g, '/')
  return c.length > 1 && c.endsWith('/') ? c.slice(0, -1) : c
}

const isReserved = (clean: string): boolean => {
  const c = clean.toLowerCase()
  return c === RESERVED_ROOT || c.startsWith(RESERVED_ROOT + '/')
}

// watchPathProblem checks a folder for the watch list.
export const watchPathProblem = (p: string): WatchPathProblem | null => {
  if (p === '') return 'empty'
  if (!p.startsWith('/')) return 'relative'
  if (utf8Length(p) > MAX_WATCH_PATH_BYTES) return 'too_long'
  for (const ch of p) {
    if (isControl(ch.codePointAt(0) ?? 0)) return 'control'
  }
  if (p.split('/').some((seg) => seg === '.' || seg === '..')) return 'dot_segment'
  if (isReserved(cleanWatchPath(p))) return 'reserved'
  return null
}

const clockMinutes = (s: string): number | null => {
  const m = /^([0-9]{2}):([0-9]{2})$/.exec(s)
  if (!m) return null
  const h = Number(m[1])
  const min = Number(m[2])
  return h > 23 || min > 59 ? null : h * 60 + min
}

// quietHoursValid accepts '' (none) or HH:MM-HH:MM in 24-hour time with two
// different ends.
export const quietHoursValid = (q: string): boolean => {
  if (q === '') return true
  if (q.length !== 11 || q[5] !== '-') return false
  const a = clockMinutes(q.slice(0, 5))
  const b = clockMinutes(q.slice(6))
  return a !== null && b !== null && a !== b
}

export type DailyTokensProblem = 'not_integer' | 'out_of_range'

// dailyTokensProblem checks the typed budget: a plain integer between
// MIN_DAILY_TOKENS and the operator's cap.
export const dailyTokensProblem = (text: string, max: number): DailyTokensProblem | null => {
  if (!/^[0-9]+$/.test(text)) return 'not_integer'
  const n = Number(text)
  return n < MIN_DAILY_TOKENS || n > max ? 'out_of_range' : null
}

// parseGoDuration reads a whole-second Go duration of hours, minutes and
// seconds as the server prints it ("15m0s"); anything else is null.
export const parseGoDuration = (s: string): number | null => {
  const m = /^(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?$/.exec(s)
  if (!m || s === '') return null
  return Number(m[1] ?? 0) * 3600 + Number(m[2] ?? 0) * 60 + Number(m[3] ?? 0)
}

export interface SweepPreset {
  value: string // what is sent (Go durations have no day unit)
  seconds: number
}

export const SWEEP_PRESETS: readonly SweepPreset[] = [
  { value: '5m', seconds: 300 },
  { value: '15m', seconds: 900 },
  { value: '1h', seconds: 3600 },
  { value: '6h', seconds: 21600 },
  { value: '24h', seconds: 86400 },
  { value: '168h', seconds: 604800 }
]

export const presetOf = (seconds: number | null): SweepPreset | undefined =>
  SWEEP_PRESETS.find((p) => p.seconds === seconds)

export interface Draft {
  proactive_enabled: boolean
  watch: string[]
  sweepSeconds: number | null // null when the stored value is unreadable
  quiet_hours: string
  dailyTokens: string // as typed
  followOperator: boolean // send null: follow the operator's cap again
  models: ModelPreferences
}

export const draftOf = (s: Settings): Draft => ({
  proactive_enabled: s.proactive_enabled,
  watch: [...s.watch],
  sweepSeconds: parseGoDuration(s.sweep_interval),
  quiet_hours: s.quiet_hours,
  dailyTokens: String(s.daily_tokens),
  followOperator: false,
  models: { chat: s.models.chat, proactive: s.models.proactive }
})

const sameList = (a: string[], b: string[]) => a.length === b.length && a.every((v, i) => v === b[i])

// buildUpdate returns the fields of the draft that differ from the settings
// it was made from, in the shape PUT /settings takes. It never carries
// anything else (no pause, dormancy or auto-approve fields).
export const buildUpdate = (base: Settings, d: Draft): SettingsUpdate => {
  const u: SettingsUpdate = {}
  if (d.proactive_enabled !== base.proactive_enabled) u.proactive_enabled = d.proactive_enabled
  if (!sameList(d.watch, base.watch)) u.watch = [...d.watch]
  const preset = presetOf(d.sweepSeconds)
  if (preset && d.sweepSeconds !== parseGoDuration(base.sweep_interval)) u.sweep_interval = preset.value
  if (d.quiet_hours !== base.quiet_hours) u.quiet_hours = d.quiet_hours
  if (d.followOperator) {
    u.daily_tokens = null
  } else if (/^[0-9]+$/.test(d.dailyTokens) && Number(d.dailyTokens) !== base.daily_tokens) {
    u.daily_tokens = Number(d.dailyTokens)
  }
  const models: NonNullable<SettingsUpdate['models']> = {}
  for (const k of ['chat', 'proactive'] as const) {
    if (d.models[k] !== base.models[k]) models[k] = d.models[k]
  }
  if (Object.keys(models).length > 0) u.models = models
  return u
}

export interface FormProblems {
  dailyTokens?: DailyTokensProblem
  quiet_hours?: true
  watch?: true
}

// formProblems lists the invalid fields of the draft (empty when it can be
// saved).
export const formProblems = (d: Draft, base: Settings): FormProblems => {
  const p: FormProblems = {}
  if (!d.followOperator) {
    const t = dailyTokensProblem(d.dailyTokens, base.daily_tokens_max)
    if (t) p.dailyTokens = t
  }
  if (!quietHoursValid(d.quiet_hours)) p.quiet_hours = true
  if (d.watch.length > MAX_WATCH || d.watch.some((w) => watchPathProblem(w) !== null)) p.watch = true
  return p
}
