import { describe, expect, it } from 'vitest'
import { statusOf } from '../test/fixtures'
import { bannerOf, budgetUsedUp, connectionNoteOf } from './banner'

const killed = { database: true, environment: false }
const used = { used_tokens: 100, daily_tokens: 100, resets_at: '2026-01-02T00:00:00Z' }

describe('bannerOf', () => {
  it.each([
    ['nothing', statusOf(), null],
    ['kill switch (database)', statusOf({ kill_switch: killed }), 'killswitch'],
    ['kill switch (environment)', statusOf({ kill_switch: { database: false, environment: true } }), 'killswitch'],
    ['kill switch over a credential pause', statusOf({ kill_switch: killed, paused: true, pause_reason: 'credential_revoked' }), 'killswitch'],
    ['credential_invalid', statusOf({ paused: true, pause_reason: 'credential_invalid' }), 'reconnect'],
    ['credential_revoked', statusOf({ paused: true, pause_reason: 'credential_revoked' }), 'reconnect'],
    ['credential_expired', statusOf({ paused: true, pause_reason: 'credential_expired' }), 'reconnect'],
    ['token_anomaly', statusOf({ paused: true, pause_reason: 'token_anomaly' }), 'token_anomaly'],
    ['unknown reason', statusOf({ paused: true, pause_reason: 'something_new' }), 'paused'],
    ['paused without reason', statusOf({ paused: true, pause_reason: '' }), 'paused'],
    ['user', statusOf({ paused: true, pause_reason: 'user' }), 'user'],
    ['dormant', statusOf({ paused: true, pause_reason: 'dormant' }), 'dormant'],
    ['budget', statusOf({ budget: used }), 'budget'],
    ['pause over budget', statusOf({ paused: true, pause_reason: 'user', budget: used }), 'user'],
    ['a reason without paused is ignored', statusOf({ paused: false, pause_reason: 'user' }), null]
  ])('%s', (_, status, kind) => {
    expect(bannerOf(status)).toBe(kind)
  })

  it('shows nothing without a status', () => {
    expect(bannerOf(null)).toBeNull()
  })
})

describe('budgetUsedUp', () => {
  it('needs a positive budget that is reached', () => {
    expect(budgetUsedUp(statusOf({ budget: { used_tokens: 99, daily_tokens: 100, resets_at: null } }))).toBe(false)
    expect(budgetUsedUp(statusOf({ budget: { used_tokens: 120, daily_tokens: 100, resets_at: null } }))).toBe(true)
    expect(budgetUsedUp(statusOf({ budget: { used_tokens: 0, daily_tokens: 0, resets_at: null } }))).toBe(false)
  })
})

describe('connectionNoteOf', () => {
  it('notes polling and offline only', () => {
    expect(connectionNoteOf('streaming')).toBeNull()
    expect(connectionNoteOf('polling')).toBe('polling')
    expect(connectionNoteOf('offline')).toBe('offline')
  })
})
