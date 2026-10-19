// Characters that can hide or reorder text: bidi embeddings, overrides and
// isolates, direction marks, the Arabic letter mark, the BOM, and every C0
// control (except tab and newline), DEL and the C1 controls.
const CONTROL = /[\u0000-\u0008\u000B-\u001F\u007F-\u009F\u061C\u200E\u200F\u202A-\u202E\u2066-\u2069\uFEFF]/g

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
