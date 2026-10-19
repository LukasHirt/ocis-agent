// coalesce runs fn once at a time: a call while it runs queues ONE more
// run, shared by every caller meanwhile. clear forgets the run in flight
// and the queued one, so the next call starts afresh.
export const coalesce = (fn: () => Promise<void>) => {
  let running: Promise<void> | null = null
  let queued: Promise<void> | null = null
  const run = (): Promise<void> => {
    if (!running) {
      const p = fn().finally(() => {
        if (running === p) running = null
      })
      running = p
      return p
    }
    if (!queued) {
      const q: Promise<void> = running
        .catch(() => undefined)
        .then(() => {
          if (queued === q) queued = null
          return run()
        })
      queued = q
    }
    return queued
  }
  return Object.assign(run, {
    clear: () => {
      running = null
      queued = null
    }
  })
}
