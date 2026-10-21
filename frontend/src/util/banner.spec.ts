import { describe, expect, it } from 'vitest'
import { statusOf } from '../test/fixtures'
import { bannerOf, budgetUsedUp, connectionChipOf, connectionNoteOf, topBannerOf } from './banner'

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

describe('topBannerOf', () => {
  const none = { serviceDown: false, statusError: null, status: null }
  it('is empty without anything to show', () => {
    expect(topBannerOf(none)).toBeNull()
    expect(topBannerOf({ ...none, status: statusOf() })).toBeNull()
  })
  it('puts an unreachable service over everything', () => {
    const status = statusOf({ kill_switch: killed })
    expect(topBannerOf({ serviceDown: true, statusError: 'x', status })).toBe('unreachable')
    expect(topBannerOf({ serviceDown: true, statusError: null, status: null })).toBe('unreachable')
  })
  it('shows a status error only together with a status, over the status banners', () => {
    expect(topBannerOf({ ...none, statusError: 'x' })).toBeNull()
    expect(topBannerOf({ ...none, statusError: 'x', status: statusOf() })).toBe('status_error')
    expect(topBannerOf({ ...none, statusError: 'x', status: statusOf({ kill_switch: killed }) })).toBe('status_error')
  })
  it('falls back to the status banner', () => {
    expect(topBannerOf({ ...none, status: statusOf({ kill_switch: killed }) })).toBe('killswitch')
    expect(topBannerOf({ ...none, status: statusOf({ budget: used }) })).toBe('budget')
  })
})

describe('connectionChipOf', () => {
  it('shows polling and offline while the service is up', () => {
    expect(connectionChipOf('streaming', false)).toBeNull()
    expect(connectionChipOf('polling', false)).toBe('polling')
    expect(connectionChipOf('offline', false)).toBe('offline')
  })
  it('shows nothing while the service is down', () => {
    expect(connectionChipOf('polling', true)).toBeNull()
    expect(connectionChipOf('offline', true)).toBeNull()
  })
})
