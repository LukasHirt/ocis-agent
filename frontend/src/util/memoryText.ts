// The server's rules for memory files, checked before a request (the
// server stays authoritative).

// A memory file holds at most MAX_FILE_BYTES bytes of UTF-8 (the server's
// default limit); larger files are shown cut and cannot be edited.
export const MAX_FILE_BYTES = 64 << 10
// The request body of an edit is at most MAX_BODY_BYTES: text dense with
// control characters is sent escaped (\u00XX) and can exceed it.
export const MAX_BODY_BYTES = 200 << 10
// A note's slug is 1..MAX_SLUG_CHARS of a-z, 0-9 and '-', not starting
// with '-'.
export const MAX_SLUG_CHARS = 64

const SLUG = /^[a-z0-9][a-z0-9-]*$/
const CORE = ['profile.md', 'projects.md']

export type SlugProblem = 'empty' | 'invalid' | 'too_long'
export type ContentProblem = 'nul' | 'invalid' | 'too_large' | 'too_large_encoded'

const encoder = new TextEncoder()
const byteLength = (s: string): number => encoder.encode(s).length

export const checkSlug = (slug: string): SlugProblem | null => {
  if (slug === '') return 'empty'
  if (!SLUG.test(slug)) return 'invalid'
  if (slug.length > MAX_SLUG_CHARS) return 'too_long'
  return null
}

export const noteName = (slug: string): string => `notes/${slug}.md`

// isMemoryName reports a name the server accepts: profile.md, projects.md
// or notes/<slug>.md.
export const isMemoryName = (name: string): boolean => {
  if (CORE.includes(name)) return true
  const m = /^notes\/(.*)\.md$/.exec(name)
  return m !== null && checkSlug(m[1]) === null
}

export const isCoreName = (name: string): boolean => CORE.includes(name)

export const CORE_NAMES: readonly string[] = CORE

// The server accepts any valid UTF-8 without NUL; a lone surrogate cannot
// be encoded as UTF-8 and is refused. Other control characters are allowed
// (they are shown as visible codes).
const LONE_SURROGATE = /[\uD800-\uDFFF]/u

export const checkContent = (s: string): ContentProblem | null => {
  if (s.includes('\u0000')) return 'nul'
  if (LONE_SURROGATE.test(s)) return 'invalid'
  if (byteLength(s) > MAX_FILE_BYTES) return 'too_large'
  if (byteLength(JSON.stringify({ content: s })) > MAX_BODY_BYTES) return 'too_large_encoded'
  return null
}

// memoryOrder sorts profile.md and projects.md first, then everything else
// by name.
export const memoryOrder = (a: string, b: string): number => {
  const ia = CORE.indexOf(a)
  const ib = CORE.indexOf(b)
  if (ia >= 0 || ib >= 0) return (ia < 0 ? CORE.length : ia) - (ib < 0 ? CORE.length : ib)
  return a < b ? -1 : a > b ? 1 : 0
}

// describeContentProblem is the fixed text of a content problem.
export const describeContentProblem = (p: ContentProblem, t: (s: string) => string): string => {
  switch (p) {
    case 'nul':
      return t('The text contains a NUL character, which a memory file cannot hold.')
    case 'invalid':
      return t('The text contains an invalid character (a lone surrogate). Remove it and save again.')
    case 'too_large':
      return t('A memory file can hold at most 64 KiB. Shorten the text and save again.')
    case 'too_large_encoded':
      return t(
        'The text has too many control characters to be sent (they take more space when sent). Remove some and save again.'
      )
  }
}
