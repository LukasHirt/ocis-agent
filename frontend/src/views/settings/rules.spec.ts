import { describe, expect, it } from 'vitest'
import {
  MAX_WATCH,
  buildUpdate,
  cleanWatchPath,
  dailyTokensProblem,
  draftOf,
  draftsEqual,
  sameSettings,
  formProblems,
  parseGoDuration,
  presetOf,
  quietHoursValid,
  watchPathProblem
} from './rules'
import { settingsOf } from '../../test/fixtures'

describe('watchPathProblem', () => {
  it.each([
    ['/', null],
    ['/Documents', null],
    ['/Photos/2024', null],
    ['/a b/c', null],
    ['/\u00dcn\u00efc\u00f6d\u00e9', null],
    ['/..foo/bar.', null],
    ['/.ocis-agentx', null],
    ['', 'empty'],
    ['Documents', 'relative'],
    ['./Documents', 'relative'],
    ['/a/../b', 'dot_segment'],
    ['/..', 'dot_segment'],
    ['/a/./b', 'dot_segment'],
    ['/a/.', 'dot_segment'],
    ['/.ocis-agent', 'reserved'],
    ['/.ocis-agent/', 'reserved'],
    ['/.ocis-agent/memory', 'reserved'],
    ['//.ocis-agent', 'reserved'],
    ['/.OCIS-Agent/notes', 'reserved'],
    ['/a\u0000b', 'control'],
    ['/a\nb', 'control'],
    ['/a\u0085b', 'control'],
    ['/a\u202eb', 'control'],
    ['/a\u2066b', 'control'],
    ['/a\u200eb', 'control'],
    ['/a\u061cb', 'control'],
    ['/a\u2028b', 'control'],
    ['/a\u007fb', 'control']
  ])('%j -> %s', (p, want) => {
    expect(watchPathProblem(p)).toBe(want)
  })

  it('limits the path to 1024 bytes (UTF-8)', () => {
    expect(watchPathProblem('/' + 'a'.repeat(1023))).toBeNull()
    expect(watchPathProblem('/' + 'a'.repeat(1024))).toBe('too_long')
    // 2 bytes each: 1 + 2 * 511 = 1023, 1 + 2 * 512 = 1025
    expect(watchPathProblem('/' + '\u00e9'.repeat(511))).toBeNull()
    expect(watchPathProblem('/' + '\u00e9'.repeat(512))).toBe('too_long')
  })

  it('allows zero-width joiners like the server', () => {
    expect(watchPathProblem('/a\u200db')).toBeNull()
  })
})

describe('cleanWatchPath', () => {
  it.each([
    ['/', '/'],
    ['/a/', '/a'],
    ['//a///b//', '/a/b'],
    ['/a', '/a']
  ])('%j -> %j', (p, want) => {
    expect(cleanWatchPath(p)).toBe(want)
  })
})

describe('quietHoursValid', () => {
  it.each([
    ['', true],
    ['22:00-07:00', true],
    ['00:00-23:59', true],
    ['07:00-22:00', true],
    ['22:00-22:00', false],
    ['24:00-07:00', false],
    ['22:60-07:00', false],
    ['2:00-07:00', false],
    ['22:00 - 07:00', false],
    ['22:00-07:00 ', false],
    ['22.00-07.00', false],
    ['aa:bb-cc:dd', false],
    ['22:00', false],
    ['\u0662\u0662:\u0660\u0660-\u0660\u0667:\u0660\u0660', false]
  ])('%j -> %s', (q, want) => {
    expect(quietHoursValid(q)).toBe(want)
  })
})

describe('dailyTokensProblem', () => {
  it.each([
    ['1000', 100000, null],
    ['100000', 100000, null],
    ['50000', 100000, null],
    ['999', 100000, 'out_of_range'],
    ['100001', 100000, 'out_of_range'],
    ['0', 100000, 'out_of_range'],
    ['', 100000, 'not_integer'],
    ['1e4', 100000, 'not_integer'],
    ['1500.5', 100000, 'not_integer'],
    ['-1000', 100000, 'not_integer'],
    ['1,000', 100000, 'not_integer'],
    [' 2000', 100000, 'not_integer'],
    ['99999999999999999999', 100000, 'out_of_range'],
    ['1000', 500, 'out_of_range']
  ])('%j with max %d -> %s', (t, max, want) => {
    expect(dailyTokensProblem(t, max)).toBe(want)
  })
})

describe('parseGoDuration', () => {
  it.each([
    ['5m0s', 300],
    ['15m0s', 900],
    ['1h0m0s', 3600],
    ['6h0m0s', 21600],
    ['24h0m0s', 86400],
    ['168h0m0s', 604800],
    ['1h30m', 5400],
    ['90s', 90],
    ['0s', 0],
    ['', null],
    ['15', null],
    ['1d', null],
    ['1.5h', null],
    ['-5m', null],
    ['m5', null]
  ])('%j -> %s', (s, want) => {
    expect(parseGoDuration(s)).toBe(want)
  })
})

describe('presetOf', () => {
  it('maps seconds to the preset the server accepts', () => {
    expect(presetOf(300)?.value).toBe('5m')
    expect(presetOf(900)?.value).toBe('15m')
    expect(presetOf(3600)?.value).toBe('1h')
    expect(presetOf(21600)?.value).toBe('6h')
    expect(presetOf(86400)?.value).toBe('24h')
    expect(presetOf(604800)?.value).toBe('168h')
    expect(presetOf(1234)).toBeUndefined()
  })
})

describe('buildUpdate', () => {
  const base = settingsOf({
    proactive_enabled: false,
    watch: ['/a'],
    sweep_interval: '15m0s',
    quiet_hours: '',
    daily_tokens: 50000,
    daily_tokens_max: 100000,
    models: { chat: null, proactive: 'm2' }
  })

  it('is empty for an untouched draft', () => {
    expect(buildUpdate(base, draftOf(base))).toEqual({})
  })

  it('carries only the changed fields', () => {
    const d = draftOf(base)
    d.quiet_hours = '22:00-07:00'
    expect(JSON.stringify(buildUpdate(base, d))).toBe('{"quiet_hours":"22:00-07:00"}')
  })

  it('sends every kind of field when changed', () => {
    const d = draftOf(base)
    d.proactive_enabled = true
    d.watch = ['/a', '/b']
    d.sweepSeconds = 3600
    d.dailyTokens = '2000'
    d.models.chat = 'm1'
    expect(JSON.stringify(buildUpdate(base, d))).toBe(
      '{"proactive_enabled":true,"watch":["/a","/b"],"sweep_interval":"1h","daily_tokens":2000,"models":{"chat":"m1"}}'
    )
  })

  it('a change back to the stored value is no change', () => {
    const d = draftOf(base)
    d.watch = ['/a', '/b']
    d.watch = ['/a']
    d.dailyTokens = '50000'
    d.sweepSeconds = 900
    d.models.proactive = 'm2'
    expect(buildUpdate(base, d)).toEqual({})
  })

  it('sends null for following the operator and for a cleared model', () => {
    const d = draftOf(base)
    d.followOperator = true
    d.models.proactive = null
    expect(JSON.stringify(buildUpdate(base, d))).toBe('{"daily_tokens":null,"models":{"proactive":null}}')
  })

  it('sends an emptied watch list', () => {
    const d = draftOf(base)
    d.watch = []
    expect(JSON.stringify(buildUpdate(base, d))).toBe('{"watch":[]}')
  })

  it('never carries read-only or unknown fields', () => {
    const d = draftOf(base)
    d.proactive_enabled = true
    d.quiet_hours = '01:00-02:00'
    const keys = Object.keys(buildUpdate(base, d))
    for (const k of ['paused', 'auto_approve_write', 'dormancy_days', 'daily_tokens_max']) {
      expect(keys).not.toContain(k)
    }
  })
})

describe('formProblems', () => {
  const base = settingsOf({ daily_tokens_max: 100000 })

  it('has none for an untouched draft', () => {
    expect(formProblems(draftOf(base), base)).toEqual({})
  })

  it('reports the invalid fields', () => {
    const d = draftOf(base)
    d.dailyTokens = '999'
    d.quiet_hours = '25:00-01:00'
    expect(formProblems(d, base)).toEqual({ dailyTokens: 'out_of_range', quiet_hours: true })
  })

  it('does not check the number while following the operator', () => {
    const d = draftOf(base)
    d.dailyTokens = 'x'
    d.followOperator = true
    expect(formProblems(d, base)).toEqual({})
  })

  it('reports a watch list over the limit', () => {
    const d = draftOf(base)
    d.watch = Array.from({ length: MAX_WATCH + 1 }, (_, i) => `/f${i}`)
    expect(formProblems(d, base)).toEqual({ watch: true })
  })
})

describe('draftsEqual', () => {
  const base = settingsOf({ watch: ['/a'], models: { chat: 'm', proactive: null } })

  it('is true for two drafts of the same settings', () => {
    expect(draftsEqual(draftOf(base), draftOf(base))).toBe(true)
  })

  it.each<[string, (d: ReturnType<typeof draftOf>) => void]>([
    ['proactive', (d) => (d.proactive_enabled = !d.proactive_enabled)],
    ['watch', (d) => d.watch.push('/b')],
    ['watch order', (d) => (d.watch = ['/b', '/a'])],
    ['sweep', (d) => (d.sweepSeconds = 300)],
    ['quiet hours', (d) => (d.quiet_hours = '01:00-02:00')],
    ['emptied daily limit', (d) => (d.dailyTokens = '')],
    ['invalid daily limit', (d) => (d.dailyTokens = '1e')],
    ['follow operator', (d) => (d.followOperator = true)],
    ['model', (d) => (d.models.chat = null)]
  ])('sees a changed %s', (_, change) => {
    const d = draftOf(base)
    change(d)
    expect(draftsEqual(d, draftOf(base))).toBe(false)
  })
})

describe('sameSettings', () => {
  it('compares by value', () => {
    expect(sameSettings(settingsOf({ watch: ['/a'] }), settingsOf({ watch: ['/a'] }))).toBe(true)
    expect(sameSettings(settingsOf({ watch: ['/a'] }), settingsOf({ watch: ['/b'] }))).toBe(false)
    expect(sameSettings(settingsOf(), settingsOf({ dormancy_days: 7 }))).toBe(false)
    expect(sameSettings(settingsOf(), settingsOf({ models: { chat: 'x', proactive: null } }))).toBe(false)
    expect(sameSettings(settingsOf(), settingsOf({ daily_tokens_max: 1 }))).toBe(false)
  })
})
