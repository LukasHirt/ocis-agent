import { describe, expect, it } from 'vitest'
import { ApiError } from '../api/errors'
import { byId, listOf, mergeHead } from './lists'

const item = (id: string) => ({ id })
const ids = (list: { id: string }[]) => list.map((x) => x.id)

describe('mergeHead', () => {
  it('replaces everything when the head reaches the end', () => {
    expect(mergeHead([item('a')], { items: [item('b')], next: null }, 'x')).toEqual({
      items: [item('b')],
      cursor: null,
      replaced: true
    })
  })

  it('keeps the older pages behind the head without duplicates', () => {
    const existing = ['m5', 'm4', 'm3', 'm2', 'm1'].map(item)
    const head = { items: ['m6', 'm5', 'm4'].map(item), next: 'm4' }
    const r = mergeHead(existing, head, 'm1-cursor')
    expect(ids(r.items)).toEqual(['m6', 'm5', 'm4', 'm3', 'm2', 'm1'])
    expect(r.cursor).toBe('m1-cursor')
    expect(r.replaced).toBe(false)
  })

  it('drops the older pages when the head does not reach them (a gap)', () => {
    const existing = ['m2', 'm1'].map(item)
    const head = { items: ['m9', 'm8'].map(item), next: 'm8' }
    const r = mergeHead(existing, head, null)
    expect(ids(r.items)).toEqual(['m9', 'm8'])
    expect(r.cursor).toBe('m8')
    expect(r.replaced).toBe(true)
  })

  it('keeps going from the head when it covers every entry read before', () => {
    const r = mergeHead(['m2', 'm1'].map(item), { items: ['m3', 'm2', 'm1'].map(item), next: 'm1' }, 'old')
    expect(ids(r.items)).toEqual(['m3', 'm2', 'm1'])
    expect(r.cursor).toBe('m1')
    expect(r.replaced).toBe(false)
  })
})

describe('listOf', () => {
  it('returns the items and a non-empty cursor', () => {
    expect(listOf({ jobs: [item('a')], next_cursor: 'a' }, 'jobs')).toEqual({ items: [item('a')], next: 'a' })
    expect(listOf({ jobs: [], next_cursor: '' }, 'jobs')).toEqual({ items: [], next: null })
    expect(listOf({ jobs: [], next_cursor: 7 }, 'jobs')).toEqual({ items: [], next: null })
  })

  it('refuses a document without the list', () => {
    for (const v of [null, {}, { jobs: 'x' }]) {
      expect(() => listOf(v, 'jobs')).toThrow(ApiError)
    }
  })
})

describe('byId', () => {
  it('collects the ids', () => {
    expect(byId(['a', 'b', 'a'].map(item))).toEqual(new Set(['a', 'b']))
  })
})
