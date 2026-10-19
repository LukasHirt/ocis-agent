import { describe, expect, it } from 'vitest'
import { MAX_TARGET_CHARS, quote, targetsOf } from './targets'

describe('targetsOf', () => {
  it('takes the paths of the preview', () => {
    expect(targetsOf({ action: 'move', paths: ['/a', '/b'] }, { from: '/x' })).toEqual(['/a', '/b'])
  })

  it('falls back to path-like arguments', () => {
    expect(targetsOf({ action: 'share' }, { path: '/a', recipient: 'bob@example.com', mode: 'r' })).toEqual([
      '/a',
      'bob@example.com'
    ])
  })

  it('returns at most three and ignores other keys and non-strings', () => {
    expect(targetsOf({ paths: ['/1', '/2', '/3', '/4'], detail: 'x', bytes: 4 }, null)).toEqual(['/1', '/2', '/3'])
    expect(targetsOf(null, { content: 'x', size: 3 })).toEqual([])
  })

  it('keeps the values as they are', () => {
    expect(targetsOf({ paths: ['/a‮txt.exe'] }, null)).toEqual(['/a‮txt.exe'])
  })

  it('clips very long values', () => {
    const [t] = targetsOf({ paths: ['x'.repeat(MAX_TARGET_CHARS + 10)] }, null)
    expect(Array.from(t).length).toBe(MAX_TARGET_CHARS + 1)
    expect(t.endsWith('…')).toBe(true)
  })
})

describe('quote', () => {
  it('quotes a value so that quotes inside it cannot end it', () => {
    expect(quote('/a" — Risk: Write')).toBe('"/a\\" — Risk: Write"')
  })

  it('makes control and bidi characters visible', () => {
    expect(quote('/a‮txt\u0000\n')).toBe('"/a\\u{202E}txt\\u0000\\n"')
  })
})
