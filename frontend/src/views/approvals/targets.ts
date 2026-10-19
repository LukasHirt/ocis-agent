import { visibleText } from '../../util/visibleText'

// The most targets a confirmation repeats, and the longest one it shows.
export const MAX_TARGETS = 3
export const MAX_TARGET_CHARS = 500

// Keys whose string values name what an action touches: paths, recipients.
const TARGET_KEY = /^(paths?|from|to|source|destination|target|recipients?|email|user|users|share_with|name)$/i

const clip = (s: string) => {
  const chars = Array.from(s)
  return chars.length > MAX_TARGET_CHARS ? chars.slice(0, MAX_TARGET_CHARS).join('') + '…' : s
}

const collect = (v: unknown, out: string[], key: string | null, depth: number) => {
  if (out.length >= MAX_TARGETS || depth > 8) return
  if (typeof v === 'string') {
    if (key !== null && TARGET_KEY.test(key) && v !== '') out.push(v)
    return
  }
  if (Array.isArray(v)) {
    for (const x of v) collect(x, out, key, depth + 1)
    return
  }
  if (v !== null && typeof v === 'object') {
    for (const [k, x] of Object.entries(v as Record<string, unknown>)) collect(x, out, k, depth + 1)
  }
}

// targetsOf returns the first path- or recipient-like strings of an
// approval's preview (or, when the preview names none, its arguments),
// with control and bidi characters made visible, for a confirmation that
// is shown as plain text.
export const targetsOf = (preview: unknown, args: unknown): string[] => {
  const out: string[] = []
  collect(preview, out, null, 0)
  if (out.length === 0) collect(args, out, null, 0)
  return out.slice(0, MAX_TARGETS).map((s) => visibleText(clip(s)).text)
}
