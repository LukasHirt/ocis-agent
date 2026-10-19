// The server's limits of chat text, checked before sending (the server
// stays authoritative).

// A message is 1..MAX_MESSAGE_BYTES bytes of UTF-8.
export const MAX_MESSAGE_BYTES = 8192
// A conversation title is at most MAX_TITLE_CHARS characters (code points).
export const MAX_TITLE_CHARS = 100

export type TextProblem = 'empty' | 'too_long' | 'control'

const encoder = new TextEncoder()

// byteLength is the length of s in UTF-8 bytes, as the server counts it.
export const byteLength = (s: string): number => encoder.encode(s).length

// A message may hold tab, newline and CR but no other C0 or C1 control,
// and no lone surrogate (it is not valid UTF-8).
// eslint-disable-next-line no-control-regex
const MESSAGE_CONTROL = /[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F-\u009F]|[\uD800-\uDFFF]/u

// A title may hold no control, line or paragraph separator and no bidi
// control.
const TITLE_CONTROL =
  // eslint-disable-next-line no-control-regex
  /[\u0000-\u001F\u007F-\u009F\u2028\u2029\u200E\u200F\u061C\u202A-\u202E\u2066-\u2069]|[\uD800-\uDFFF]/u

// checkMessage returns why the server would refuse s as a message, or
// null. Blank text counts as empty: there is nothing to answer.
export const checkMessage = (s: string): TextProblem | null => {
  if (s.trim() === '') return 'empty'
  if (MESSAGE_CONTROL.test(s)) return 'control'
  if (byteLength(s) > MAX_MESSAGE_BYTES) return 'too_long'
  return null
}

// checkTitle returns why s is not a usable title (1..MAX_TITLE_CHARS
// characters, checked as given: trim first), or null.
export const checkTitle = (s: string): TextProblem | null => {
  if (s.trim() === '') return 'empty'
  if (TITLE_CONTROL.test(s)) return 'control'
  if (Array.from(s).length > MAX_TITLE_CHARS) return 'too_long'
  return null
}
