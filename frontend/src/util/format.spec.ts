import { describe, expect, it } from 'vitest'
import { formatBytes, formatDuration, formatTime, shortId } from './format'

describe('formatDuration', () => {
  it('formats milliseconds as the two largest units', () => {
    expect(formatDuration(0)).toBe('0s')
    expect(formatDuration(999)).toBe('0s')
    expect(formatDuration(45_000)).toBe('45s')
    expect(formatDuration(90_000)).toBe('1m 30s')
    expect(formatDuration(3_600_000)).toBe('1h')
    expect(formatDuration(3_660_000)).toBe('1h 1m')
    expect(formatDuration(3 * 86_400_000 + 2 * 3_600_000 + 5_000)).toBe('3d 2h')
  })

  it('returns an empty string for invalid input', () => {
    expect(formatDuration(-1)).toBe('')
    expect(formatDuration(Number.NaN)).toBe('')
    expect(formatDuration(Number.POSITIVE_INFINITY)).toBe('')
    expect(formatDuration('5' as unknown as number)).toBe('')
  })
})

describe('formatBytes', () => {
  it('uses binary units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(200 * 1024)).toBe('200 KiB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5 MiB')
    expect(formatBytes(3 * 1024 ** 3)).toBe('3 GiB')
    expect(formatBytes(2 * 1024 ** 5)).toBe('2048 TiB')
  })

  it('returns an empty string for invalid input', () => {
    expect(formatBytes(-1)).toBe('')
    expect(formatBytes(Number.NaN)).toBe('')
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('')
    expect(formatBytes(null as unknown as number)).toBe('')
  })
})

describe('formatTime', () => {
  it('formats a valid timestamp with Intl', () => {
    const iso = '2026-01-02T03:04:05Z'
    const expected = new Intl.DateTimeFormat('en-GB', {
      dateStyle: 'medium',
      timeStyle: 'short'
    }).format(new Date(iso))
    expect(formatTime(iso, 'en-GB')).toBe(expected)
  })

  it('uses the given locale', () => {
    const iso = '2026-01-02T03:04:05Z'
    expect(formatTime(iso, 'de-DE')).not.toBe(formatTime(iso, 'en-US'))
  })

  it('falls back to the raw string for invalid dates', () => {
    expect(formatTime('not a date')).toBe('not a date')
    expect(formatTime('')).toBe('')
    expect(formatTime(undefined as unknown as string)).toBe('')
  })

  it('falls back to a default locale for an invalid locale tag', () => {
    const iso = '2026-01-02T03:04:05Z'
    expect(formatTime(iso, 'not_a_locale!!')).not.toBe(iso)
  })
})

describe('shortId', () => {
  it('keeps the first eight characters', () => {
    expect(shortId('0123456789abcdef')).toBe('01234567')
    expect(shortId('abc')).toBe('abc')
  })

  it('returns an empty string for missing ids', () => {
    expect(shortId('')).toBe('')
    expect(shortId(undefined as unknown as string)).toBe('')
  })

  it('does not split a surrogate pair', () => {
    expect(shortId('\u{1F600}'.repeat(10))).toBe('\u{1F600}'.repeat(8))
  })
})
