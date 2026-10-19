import { describe, expect, it } from 'vitest'
import {
  MAX_BODY_BYTES,
  MAX_FILE_BYTES,
  checkContent,
  checkSlug,
  describeContentProblem,
  isMemoryName,
  memoryOrder,
  noteName
} from './memoryText'

describe('memory text rules', () => {
  it('accepts the names the server accepts', () => {
    expect(isMemoryName('profile.md')).toBe(true)
    expect(isMemoryName('projects.md')).toBe(true)
    expect(isMemoryName('notes/a.md')).toBe(true)
    expect(isMemoryName('notes/0-trip-2026.md')).toBe(true)
    expect(isMemoryName('notes/' + 'a'.repeat(64) + '.md')).toBe(true)
  })

  it('refuses every other name', () => {
    for (const n of [
      '',
      'README.md',
      'Profile.md',
      'notes/.md',
      'notes/-a.md',
      'notes/A.md',
      'notes/a_b.md',
      'notes/a.txt',
      'notes/a/b.md',
      'notes/../profile.md',
      'notes/' + 'a'.repeat(65) + '.md',
      'other/a.md'
    ]) {
      expect(isMemoryName(n), n).toBe(false)
    }
  })

  it('checks a note slug as typed', () => {
    expect(checkSlug('')).toBe('empty')
    expect(checkSlug('trip-2026')).toBeNull()
    expect(checkSlug('Trip')).toBe('invalid')
    expect(checkSlug('-a')).toBe('invalid')
    expect(checkSlug('a b')).toBe('invalid')
    expect(checkSlug('a.md')).toBe('invalid')
    expect(checkSlug('a'.repeat(64))).toBeNull()
    expect(checkSlug('a'.repeat(65))).toBe('too_long')
    expect(noteName('trip')).toBe('notes/trip.md')
  })

  it('accepts text with any control character except NUL', () => {
    expect(checkContent('')).toBeNull()
    expect(checkContent('a\tb\r\nc\u0001\u001b[31m\u202E')).toBeNull()
    expect(checkContent('a\u0000b')).toBe('nul')
  })

  it('refuses a lone surrogate, which is not UTF-8', () => {
    expect(checkContent('a\ud800b')).toBe('invalid')
    expect(checkContent('a\udc00')).toBe('invalid')
    expect(checkContent('😀')).toBeNull()
  })

  it('counts the size in UTF-8 bytes', () => {
    expect(checkContent('a'.repeat(MAX_FILE_BYTES))).toBeNull()
    expect(checkContent('a'.repeat(MAX_FILE_BYTES + 1))).toBe('too_large')
    // 3 bytes each
    expect(checkContent('€'.repeat(Math.floor(MAX_FILE_BYTES / 3)))).toBeNull()
    expect(checkContent('€'.repeat(Math.floor(MAX_FILE_BYTES / 3) + 1))).toBe('too_large')
  })

  it('refuses a file whose request body would exceed the server limit', () => {
    // a control character is sent as \u00XX: 6 bytes
    const dense = '\u0001'.repeat(Math.ceil(MAX_BODY_BYTES / 6) + 1)
    expect(checkContent(dense)).toBe('too_large_encoded')
  })

  it('orders profile and projects first, then the notes by name', () => {
    const names = ['notes/b.md', 'projects.md', 'notes/a.md', 'profile.md']
    expect([...names].sort(memoryOrder)).toEqual(['profile.md', 'projects.md', 'notes/a.md', 'notes/b.md'])
  })
})

describe('describeContentProblem', () => {
  it('has a fixed text for every problem', () => {
    for (const p of ['nul', 'invalid', 'too_large', 'too_large_encoded'] as const) {
      expect(describeContentProblem(p, (s) => s)).toMatch(/\.$/)
    }
  })
})
