import { describe, expect, it } from 'vitest'
import { groupByDay } from './days'

const at = (y: number, mo: number, d: number, h = 12) => new Date(y, mo - 1, d, h).toISOString()

describe('groupByDay', () => {
  it('puts events of one local day into one group', () => {
    const g = groupByDay([{ ts: at(2026, 1, 5, 9) }, { ts: at(2026, 1, 5, 18) }], 'Unknown date', 'en-US')
    expect(g).toHaveLength(1)
    expect(g[0].key).toBe('2026-01-05')
    expect(g[0].items).toHaveLength(2)
    expect(g[0].label).toBe('Monday, January 5, 2026')
  })

  it('starts a new group for the next day and keeps the order', () => {
    const items = [{ ts: at(2026, 1, 6) }, { ts: at(2026, 1, 5) }, { ts: at(2026, 1, 5, 8) }]
    const g = groupByDay(items, 'Unknown date', 'en-US')
    expect(g.map((x) => x.key)).toEqual(['2026-01-06', '2026-01-05'])
    expect(g[1].items).toEqual([items[1], items[2]])
  })

  it('groups consecutive events only', () => {
    const g = groupByDay([{ ts: at(2026, 1, 5) }, { ts: at(2026, 1, 4) }, { ts: at(2026, 1, 5) }], 'x', 'en-US')
    expect(g.map((x) => x.key)).toEqual(['2026-01-05', '2026-01-04', '2026-01-05'])
  })

  it('files an invalid timestamp under the unknown label', () => {
    const g = groupByDay([{ ts: 'nope' }, { ts: '' }], 'Unknown date', 'en-US')
    expect(g).toEqual([{ key: '', label: 'Unknown date', items: [{ ts: 'nope' }, { ts: '' }] }])
  })
})
