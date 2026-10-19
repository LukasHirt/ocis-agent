import { describe, expect, it } from 'vitest'
import { visibleSegments, visibleText } from './visibleText'

describe('visibleText', () => {
  it('leaves plain text, tabs and newlines alone', () => {
    expect(visibleText('a\tb\nc')).toEqual({ text: 'a\tb\nc', hadControls: false })
  })

  it('makes bidi overrides and isolates visible', () => {
    expect(visibleText('evil\u202Etxt.exe')).toEqual({
      text: 'evil\\u{202E}txt.exe',
      hadControls: true
    })
    for (const c of [0x202a, 0x202b, 0x202c, 0x202d, 0x2066, 0x2067, 0x2068, 0x2069]) {
      const hex = c.toString(16).toUpperCase()
      expect(visibleText(String.fromCodePoint(c)).text).toBe(`\\u{${hex}}`)
    }
  })

  it('makes direction marks, the Arabic letter mark and the BOM visible', () => {
    expect(visibleText('\u200E\u200F\u061C\uFEFF').text).toBe(
      '\\u{200E}\\u{200F}\\u{061C}\\u{FEFF}'
    )
  })

  it('makes NUL, other C0 controls, DEL and C1 controls visible', () => {
    expect(visibleText('a\u0000b').text).toBe('a\\u{0000}b')
    expect(visibleText('\r').text).toBe('\\u{000D}')
    expect(visibleText('\u001B[31m').text).toBe('\\u{001B}[31m')
    expect(visibleText('\u007F').text).toBe('\\u{007F}')
    expect(visibleText('\u0085\u009B').text).toBe('\\u{0085}\\u{009B}')
  })

  it('keeps characters outside the listed classes, such as the zero-width space and emoji', () => {
    expect(visibleText('a\u200Bb')).toEqual({ text: 'a\u200Bb', hadControls: false })
    expect(visibleText('ok \u{1F600} \u00E9')).toEqual({ text: 'ok \u{1F600} \u00E9', hadControls: false })
  })

  it('keeps markup as literal text', () => {
    const s = '<script>alert(1)</script></data>&amp;&lt;'
    expect(visibleText(s)).toEqual({ text: s, hadControls: false })
  })

  it('handles very long strings', () => {
    const s = 'x'.repeat(200_000) + '\u202E' + 'y'.repeat(200_000)
    const r = visibleText(s)
    expect(r.hadControls).toBe(true)
    expect(r.text.length).toBe(400_000 + '\\u{202E}'.length)
  })

  it('handles the empty string', () => {
    expect(visibleText('')).toEqual({ text: '', hadControls: false })
    expect(visibleSegments('')).toEqual([])
  })
})

describe('visibleSegments', () => {
  it('splits text into plain runs and one segment per control character', () => {
    expect(visibleSegments('ab\u202E\u0000cd\n')).toEqual([
      { ctl: false, text: 'ab' },
      { ctl: true, text: '\\u{202E}', code: 'U+202E' },
      { ctl: true, text: '\\u{0000}', code: 'U+0000' },
      { ctl: false, text: 'cd\n' }
    ])
  })
})
