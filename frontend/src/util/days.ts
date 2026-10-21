export interface DayGroup<T> {
  key: string
  label: string
  items: T[]
}

const pad = (n: number): string => String(n).padStart(2, '0')

// groupByDay groups CONSECUTIVE items by the local calendar day of their
// `ts`, keeping the order. An invalid `ts` goes to a group with key ''.
export const groupByDay = <T extends { ts: string }>(items: T[], unknownLabel: string, locale?: string): DayGroup<T>[] => {
  const fmt = new Intl.DateTimeFormat(locale, { dateStyle: 'full' })
  const out: DayGroup<T>[] = []
  for (const item of items) {
    const d = new Date(item.ts)
    const valid = item.ts !== '' && !Number.isNaN(d.getTime())
    const key = valid ? `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` : ''
    const last = out[out.length - 1]
    if (last && last.key === key) last.items.push(item)
    else out.push({ key, label: valid ? fmt.format(d) : unknownLabel, items: [item] })
  }
  return out
}
