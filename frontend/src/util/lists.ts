import { ApiError } from '../api/errors'

// Helpers for the API's keyset-paged lists (newest first, an id cursor).

// byId collects the ids of a list.
export const byId = <T extends { id: string }>(list: T[]): Set<string> => new Set(list.map((x) => x.id))

// listOf reads a page: the items under key and the cursor (null when
// absent or empty). A document without the list is a bad answer.
export const listOf = <T>(v: unknown, key: string): { items: T[]; next: string | null } => {
  const doc = v as Record<string, unknown> | null
  const items = doc?.[key]
  if (!Array.isArray(items)) throw new ApiError(502, 'internal')
  const next = doc?.next_cursor
  return { items: items as T[], next: typeof next === 'string' && next !== '' ? next : null }
}

// mergeHead merges a freshly read first page into a list read before (both
// newest first): entries the head does not hold are kept behind it, as long
// as the head reaches into the old list; otherwise there is a gap and the
// older entries are dropped. The cursor for older entries is the old one
// when old entries are kept, else the head's. replaced tells that the head
// replaced the list: a page of older entries read meanwhile is stale.
export const mergeHead = <T extends { id: string }>(
  existing: T[],
  head: { items: T[]; next: string | null },
  olderCursor: string | null
): { items: T[]; cursor: string | null; replaced: boolean } => {
  if (head.next === null || head.items.length === 0) return { items: head.items, cursor: head.next, replaced: true }
  const last = head.items[head.items.length - 1].id
  const at = existing.findIndex((x) => x.id === last)
  if (at < 0) return { items: head.items, cursor: head.next, replaced: true }
  const seen = byId(head.items)
  const tail = existing.slice(at + 1).filter((x) => !seen.has(x.id))
  return { items: [...head.items, ...tail], cursor: tail.length > 0 ? olderCursor : head.next, replaced: false }
}
