import { describe, expect, it } from 'vitest'
import { coalesce } from './coalesce'

const deferred = () => {
  let resolve!: () => void
  const promise = new Promise<void>((r) => (resolve = r))
  return { promise, resolve }
}

describe('coalesce', () => {
  it('runs once at a time and queues one follow-up shared by every caller meanwhile', async () => {
    const gates = [deferred(), deferred()]
    let runs = 0
    const run = coalesce(async () => {
      await gates[runs++].promise
    })
    const a = run()
    const b = run()
    const c = run()
    expect(b).toBe(c)
    expect(runs).toBe(1)
    gates[0].resolve()
    await a
    await Promise.resolve()
    expect(runs).toBe(2)
    gates[1].resolve()
    await b
    expect(runs).toBe(2)
  })

  it('starts afresh after clear', async () => {
    const gate = deferred()
    let runs = 0
    const run = coalesce(async () => {
      runs++
      if (runs === 1) await gate.promise
    })
    void run()
    run.clear()
    await run()
    expect(runs).toBe(2)
    gate.resolve()
  })

  it('drops a follow-up queued before clear', async () => {
    const gate = deferred()
    let runs = 0
    const run = coalesce(async () => {
      runs++
      if (runs === 1) await gate.promise
    })
    const first = run()
    const queued = run()
    run.clear()
    gate.resolve()
    await first
    await queued
    expect(runs).toBe(1)
    await run()
    expect(runs).toBe(2)
  })

  it('runs again after a failed run', async () => {
    let runs = 0
    const run = coalesce(async () => {
      runs++
      if (runs === 1) throw new Error('boom')
    })
    await expect(run()).rejects.toThrow('boom')
    await run()
    expect(runs).toBe(2)
  })
})
