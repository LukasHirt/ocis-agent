import { describe, expect, it } from 'vitest'
import { prettyJson, stringifyValue } from './json'

describe('prettyJson', () => {
  it('returns null for invalid JSON', () => {
    expect(prettyJson('')).toBeNull()
    expect(prettyJson('{')).toBeNull()
    expect(prettyJson('{"a":1,}')).toBeNull()
  })

  it('keeps empty containers on one line', () => {
    expect(prettyJson('{"a":{},"b":[ ]}')).toBe('{\n  "a": {},\n  "b": []\n}')
  })

  it('copies strings with structural characters, escapes and whitespace verbatim', () => {
    const raw = '{"k":"a, b: {c} [d] \\"e\\" \\\\","s":"  x  "}'
    expect(prettyJson(raw)).toBe('{\n  "k": "a, b: {c} [d] \\"e\\" \\\\",\n  "s": "  x  "\n}')
  })

  it('re-indents already indented input', () => {
    expect(prettyJson('[\n\t1 ,\r\n 2 ]')).toBe('[\n  1,\n  2\n]')
  })

  it('handles scalars', () => {
    expect(prettyJson(' "x" ')).toBe('"x"')
    expect(prettyJson('-1.5e3')).toBe('-1.5e3')
    expect(prettyJson('null')).toBe('null')
  })
})

describe('stringifyValue', () => {
  it('falls back to text for values JSON cannot represent', () => {
    expect(stringifyValue(undefined)).toBe('undefined')
    expect(stringifyValue(10n)).toBe('10')
    const cyclic: Record<string, unknown> = {}
    cyclic.self = cyclic
    expect(stringifyValue(cyclic)).toBe('[object Object]')
  })
})
