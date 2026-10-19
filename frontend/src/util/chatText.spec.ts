import { describe, expect, it } from 'vitest'
import {
  MAX_MESSAGE_BYTES,
  MAX_TITLE_CHARS,
  byteLength,
  checkMessage,
  checkTitle
} from './chatText'

describe('byteLength', () => {
  it('counts UTF-8 bytes, not UTF-16 units', () => {
    expect(byteLength('abc')).toBe(3)
    expect(byteLength('é')).toBe(2)
    expect(byteLength('€')).toBe(3)
    expect(byteLength('😀')).toBe(4) // two UTF-16 units
    expect(byteLength('')).toBe(0)
  })
})

describe('checkMessage', () => {
  it('accepts 1..8192 bytes with tab, newline and carriage return', () => {
    expect(checkMessage('a')).toBeNull()
    expect(checkMessage('line\n\tnext\r\n')).toBeNull()
    expect(checkMessage('a'.repeat(MAX_MESSAGE_BYTES))).toBeNull()
  })

  it('refuses empty and blank text', () => {
    expect(checkMessage('')).toBe('empty')
    expect(checkMessage(' \n\t')).toBe('empty')
  })

  it('counts the limit in bytes of multi-byte text', () => {
    // 2731 euro signs are 8193 bytes but only 2731 characters
    expect(checkMessage('€'.repeat(2730))).toBeNull()
    expect(checkMessage('€'.repeat(2731))).toBe('too_long')
    expect(checkMessage('a'.repeat(MAX_MESSAGE_BYTES + 1))).toBe('too_long')
  })

  it('refuses control characters other than tab, newline and CR, and lone surrogates', () => {
    for (const c of ['\u0000', '\u0007', '\u000b', '\u001b', '\u007f', '\u0085', '\u009f']) {
      expect(checkMessage('a' + c + 'b')).toBe('control')
    }
    expect(checkMessage('a\ud800b')).toBe('control')
    expect(checkMessage('a\udc00')).toBe('control')
    // the server allows bidi and format characters in messages
    expect(checkMessage('a\u202eb\u200b')).toBeNull()
  })
})

describe('checkTitle', () => {
  it('accepts 1..100 characters', () => {
    expect(checkTitle('Plans')).toBeNull()
    expect(checkTitle('😀'.repeat(MAX_TITLE_CHARS))).toBeNull()
    expect(checkTitle('x'.repeat(MAX_TITLE_CHARS + 1))).toBe('too_long')
  })

  it('refuses an empty or blank title', () => {
    expect(checkTitle('')).toBe('empty')
    expect(checkTitle('   ')).toBe('empty')
  })

  it('refuses control, line separator and bidi characters', () => {
    for (const c of ['\n', '\t', '\u0085', '\u2028', '\u2029', '\u200e', '\u200f', '\u061c', '\u202a', '\u202e', '\u2066', '\u2069', '\ud800']) {
      expect(checkTitle('a' + c)).toBe('control')
    }
  })
})
