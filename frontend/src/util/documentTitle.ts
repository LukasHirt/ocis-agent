import { APPID } from '../appid'

const MAX = 60

// cleanTitlePart makes untrusted text safe for the document title: control,
// invisible and direction characters are removed (not shown as escapes),
// whitespace is collapsed, and the result is cut at 60 characters with an
// ellipsis.
export const cleanTitlePart = (s: string): string => {
  const t = s
    .replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F-\u009F\u00AD\u034F\u061C\u115F\u1160\u200B-\u200F\u2028-\u202E\u2060-\u2064\u2066-\u2069\u3164\uFEFF\u{E0000}-\u{E007F}\uD800-\uDFFF]/gu, '')
    .replace(/\s+/g, ' ')
    .trim()
  const chars = [...t]
  return chars.length > MAX ? chars.slice(0, MAX - 1).join('') + '…' : t
}

export interface TitleInput {
  name?: string | symbol | null // the host-prefixed route name
  params?: Record<string, unknown>
  section: string // the route's translated meta.title
  app: string // translated 'Agent'
  task: string // translated 'Task'
  enrolled: boolean
  conversationTitle?: string
}

// documentTitleSegments returns the title parts from most to least specific.
// The specific part is only ever a conversation title or a memory file name,
// both cleaned; a task gets a fixed label, never its goal.
export const documentTitleSegments = (i: TitleInput): string[] => {
  if (!i.enrolled) return [i.app]
  const out: string[] = []
  const section = cleanTitlePart(i.section)
  let specific = ''
  if (i.name === `${APPID}-chat-conversation`) specific = cleanTitlePart(i.conversationTitle ?? '')
  else if (i.name === `${APPID}-tasks-job`) specific = cleanTitlePart(i.task)
  else if (i.name === `${APPID}-memory-file`) {
    const n = i.params?.name
    specific = cleanTitlePart(Array.isArray(n) ? n.map(String).join('/') : typeof n === 'string' ? n : '')
  }
  if (specific) out.push(specific)
  if (section && section !== i.app) out.push(section)
  out.push(i.app)
  return out
}
