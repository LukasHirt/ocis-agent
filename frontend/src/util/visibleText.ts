// Characters that can hide, fake or reorder text, all shown as escapes:
// - every C0 control except tab and newline, DEL and the C1 controls
// - bidi embeddings, overrides and isolates, direction marks, the Arabic
//   letter mark, line and paragraph separators
// - invisible characters: soft hyphen, combining grapheme joiner, zero-width
//   space, non-joiner and joiner, word joiner and invisible operators, BOM,
//   tag characters, Hangul fillers
// - lone surrogates (the u flag matches only unpaired ones)
// A zero-width joiner between two pictographs (an emoji sequence such as a
// family) is kept, optionally after a variation selector or skin tone.
const CONTROL =
  /(?!(?<=\p{Extended_Pictographic}[\uFE0F\u{1F3FB}-\u{1F3FF}]?)\u200D(?=\p{Extended_Pictographic}))[\u0000-\u0008\u000B-\u001F\u007F-\u009F\u00AD\u034F\u061C\u115F\u1160\u200B-\u200F\u2028-\u202E\u2060-\u2064\u2066-\u2069\u3164\uFEFF\u{E0000}-\u{E007F}\uD800-\uDFFF]/gu

export type Segment = { ctl: false; text: string } | { ctl: true; text: string; code: string }

const hex = (c: string) => c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, '0')

// visibleSegments splits s into runs of plain text and one segment per
// control character, whose text is its visible escape (\u{XXXX}).
export const visibleSegments = (s: string): Segment[] => {
  const out: Segment[] = []
  let last = 0
  for (const m of s.matchAll(CONTROL)) {
    const i = m.index!
    if (i > last) out.push({ ctl: false, text: s.slice(last, i) })
    const h = hex(m[0])
    out.push({ ctl: true, text: `\\u{${h}}`, code: `U+${h}` })
    last = i + m[0].length
  }
  if (last < s.length) out.push({ ctl: false, text: s.slice(last) })
  return out
}

// visibleText returns s with every control character replaced by its
// visible escape, and whether there was any.
export const visibleText = (s: string): { text: string; hadControls: boolean } => {
  let hadControls = false
  const text = s.replace(CONTROL, (c) => {
    hadControls = true
    return `\\u{${hex(c)}}`
  })
  return { text, hadControls }
}
