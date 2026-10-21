import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, approvalOf, json } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import { jobOf, jobsHandler, jobsState, stepOf, type JobsState } from '../test/jobs'
import { JOB_FILTERS, JOB_PAGE, useJobsStore } from './jobs'

let detach: (() => void) | undefined
afterEach(() => {
  detach?.()
  detach = undefined
})

const setup = async (st: JobsState = jobsState(), extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? jobsHandler(st)(c))
  const agent = await startStore(server)
  const jobs = useJobsStore()
  detach = jobs.attach()
  return { st, server, jobs, ...agent }
}

const ids = (list: { id: string }[]) => list.map((x) => x.id)
const many = (n: number) => Array.from({ length: n }, (_, i) => jobOf('j' + i))
const jobReads = (server: ReturnType<typeof fakeServer>) =>
  server.calls.filter((c) => c.method === 'GET' && c.path === '/jobs')

describe('useJobsStore', () => {
  it('offers every state of the API as a filter, after all', () => {
    expect(JOB_FILTERS).toEqual(['', 'queued', 'running', 'waiting_approval', 'succeeded', 'failed', 'cancelled'])
  })

  it('loads the jobs newest first and more with the cursor', async () => {
    const st = jobsState({ jobs: many(JOB_PAGE + 10) })
    const { jobs, server } = await setup(st)
    await jobs.refresh()
    expect(jobs.jobs).toHaveLength(JOB_PAGE)
    expect(jobs.cursor).toBe('j' + (JOB_PAGE - 1))
    expect(jobs.loaded).toBe(true)
    await jobs.loadMore()
    expect(ids(jobs.jobs)).toEqual(ids(st.jobs))
    expect(jobs.cursor).toBeNull()
    const last = jobReads(server).at(-1)!
    expect(last.query.get('cursor')).toBe('j' + (JOB_PAGE - 1))
    expect(last.query.get('limit')).toBe(String(JOB_PAGE))
    expect(last.query.has('state')).toBe(false)
  })

  it('keeps following an empty page that still carries a cursor', async () => {
    let n = 0
    const { jobs } = await setup(jobsState(), (c) => {
      if (c.method !== 'GET' || c.path !== '/jobs') return undefined
      n++
      if (n === 1) return json({ jobs: [], next_cursor: 'k1' })
      return json({ jobs: [jobOf('j1')], next_cursor: null })
    })
    await jobs.refresh()
    expect(jobs.jobs).toEqual([])
    expect(jobs.cursor).toBe('k1')
    await jobs.loadMore()
    expect(ids(jobs.jobs)).toEqual(['j1'])
    expect(jobs.cursor).toBeNull()
  })

  it('does not repeat jobs that two pages both hold', async () => {
    let n = 0
    const { jobs } = await setup(jobsState(), (c) => {
      if (c.method !== 'GET' || c.path !== '/jobs') return undefined
      n++
      if (n === 1) return json({ jobs: [jobOf('j1'), jobOf('j2')], next_cursor: 'j2' })
      return json({ jobs: [jobOf('j2'), jobOf('j3')], next_cursor: null })
    })
    await jobs.refresh()
    await jobs.loadMore()
    expect(ids(jobs.jobs)).toEqual(['j1', 'j2', 'j3'])
  })

  it('filters by the API state and starts over when the filter changes', async () => {
    const st = jobsState({
      jobs: [jobOf('j1', { state: 'failed' }), jobOf('j2', { state: 'running' }), jobOf('j3', { state: 'failed' })]
    })
    const { jobs, server } = await setup(st)
    await jobs.refresh()
    expect(ids(jobs.jobs)).toEqual(['j1', 'j2', 'j3'])
    await jobs.setFilter('failed')
    expect(jobs.filter).toBe('failed')
    expect(ids(jobs.jobs)).toEqual(['j1', 'j3'])
    expect(jobReads(server).at(-1)!.query.get('state')).toBe('failed')
    await jobs.setFilter('')
    expect(ids(jobs.jobs)).toEqual(['j1', 'j2', 'j3'])
    expect(jobReads(server).at(-1)!.query.has('state')).toBe(false)
  })

  it('refuses a filter the API does not know', async () => {
    const { jobs, server } = await setup()
    await jobs.refresh()
    const n = jobReads(server).length
    await jobs.setFilter('bogus')
    expect(jobs.filter).toBe('')
    expect(jobReads(server).length).toBe(n)
  })

  it('drops the answer of a filter that was replaced meanwhile', async () => {
    let release: (() => void) | undefined
    const st = jobsState({ jobs: [jobOf('j1', { state: 'failed' }), jobOf('j2', { state: 'running' })] })
    const { jobs } = await setup(st, (c) => {
      if (c.path === '/jobs' && c.query.get('state') === 'failed') {
        return new Promise<Response>((resolve) => {
          release = () => resolve(json({ jobs: [jobOf('j1', { state: 'failed' })], next_cursor: null }))
        })
      }
      return undefined
    })
    const first = jobs.setFilter('failed')
    await flushPromises()
    const second = jobs.setFilter('running')
    await second
    release!()
    await first
    expect(jobs.filter).toBe('running')
    expect(ids(jobs.jobs)).toEqual(['j2'])
  })

  it('keeps a fixed error and the old list when loading fails', async () => {
    let fail = false
    const { jobs } = await setup(jobsState({ jobs: [jobOf('j1')] }), (c) =>
      fail && c.path === '/jobs' ? apiErr(503, 'unavailable') : undefined
    )
    await jobs.refresh()
    fail = true
    await jobs.refresh()
    expect(ids(jobs.jobs)).toEqual(['j1'])
    expect(jobs.error).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await jobs.refresh()
    expect(jobs.error).toBeNull()
  })

  it('merges a fresh first page into the pages read before', async () => {
    const st = jobsState({ jobs: many(JOB_PAGE + 5) })
    const { jobs } = await setup(st)
    await jobs.refresh()
    await jobs.loadMore()
    st.jobs.unshift(jobOf('new'))
    st.jobs[1] = { ...st.jobs[1], state: 'failed' }
    await jobs.refresh()
    expect(jobs.jobs).toHaveLength(JOB_PAGE + 6)
    expect(jobs.jobs[0].id).toBe('new')
    expect(jobs.jobs[1].state).toBe('failed')
    expect(jobs.cursor).toBeNull()
  })

  it('drops a page of older jobs read while a refresh replaced the list', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let n = 0
    const { jobs } = await setup(jobsState(), (c) => {
      if (c.method !== 'GET' || c.path !== '/jobs') return undefined
      n++
      if (n === 1) return json({ jobs: [jobOf('j2'), jobOf('j1')], next_cursor: 'j1' })
      if (c.query.get('cursor') === 'j1') return gate.then(() => json({ jobs: [jobOf('j0')], next_cursor: null }))
      // the head no longer reaches the jobs read before: a gap
      return json({ jobs: [jobOf('k9'), jobOf('k8')], next_cursor: 'k8' })
    })
    await jobs.refresh()
    const more = jobs.loadMore()
    await flushPromises()
    await jobs.refresh()
    expect(ids(jobs.jobs)).toEqual(['k9', 'k8'])
    release()
    await more
    expect(ids(jobs.jobs)).toEqual(['k9', 'k8'])
    expect(jobs.cursor).toBe('k8')
    expect(jobs.moreLoading).toBe(false)
  })

  it('refreshes the first page on a jobs change, not on other kinds', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
    const { jobs, server, events } = await setup(st)
    await jobs.refresh()
    const n = jobReads(server).length
    await events().onChanged(['settings'])
    await flushPromises()
    expect(jobReads(server).length).toBe(n)
    st.jobs[0] = { ...st.jobs[0], state: 'succeeded' }
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(jobs.jobs[0].state).toBe('succeeded')
    expect(jobReads(server).at(-1)!.query.has('cursor')).toBe(false)
  })

  it('reads again on a ready and keeps the filter', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'failed' })] })
    const { jobs, server, events } = await setup(st)
    await jobs.setFilter('failed')
    st.jobs.unshift(jobOf('j2', { state: 'failed' }))
    await events().onReady()
    await flushPromises()
    expect(ids(jobs.jobs)).toEqual(['j2', 'j1'])
    expect(jobReads(server).at(-1)!.query.get('state')).toBe('failed')
  })

  it('does nothing on events before the list was ever read or after detaching', async () => {
    const { server, events } = await setup(jobsState({ jobs: [jobOf('j1')] }))
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(jobReads(server)).toHaveLength(0)
    detach!()
    detach = undefined
    await events().onReady()
    await flushPromises()
    expect(jobReads(server)).toHaveLength(0)
  })

  it('reuses a refresh in flight and queues one follow-up', async () => {
    const releases: (() => void)[] = []
    const { jobs, server } = await setup(jobsState(), (c) => {
      if (c.path !== '/jobs') return undefined
      return new Promise<Response>((resolve) => {
        releases.push(() => resolve(json({ jobs: [], next_cursor: null })))
      })
    })
    const a = jobs.refresh()
    const b = jobs.refresh()
    const c = jobs.refresh()
    await flushPromises()
    expect(jobReads(server)).toHaveLength(1)
    releases.shift()!()
    await flushPromises()
    expect(jobReads(server)).toHaveLength(2)
    releases.shift()!()
    await Promise.all([a, b, c])
    expect(jobReads(server)).toHaveLength(2)
  })

  describe('detail', () => {
    it('opens a job with its steps and the pending approvals of a live job', async () => {
      const st = jobsState({
        jobs: [jobOf('j1', { state: 'waiting_approval' })],
        steps: { j1: [stepOf(1, { tokens_in: 10, tokens_out: 5 }), stepOf(2, { kind: 'tool', tokens_in: 0 })] },
        approvals: { j1: [approvalOf('a1', { job_id: 'j1' })] }
      })
      const { jobs, server } = await setup(st)
      await jobs.openJob('j1')
      expect(jobs.detail?.id).toBe('j1')
      expect(jobs.detail?.steps.map((s) => s.n)).toEqual([1, 2])
      expect(jobs.detailApprovals?.items.map((a) => a.id)).toEqual(['a1'])
      const ap = server.calls.find((c) => c.path === '/approvals' && c.query.has('job_id'))!
      expect(ap.query.get('job_id')).toBe('j1')
    })

    it('reads no approvals for an ended job', async () => {
      const { jobs, server } = await setup(jobsState({ jobs: [jobOf('j1', { state: 'succeeded' })] }))
      await jobs.openJob('j1')
      expect(server.calls.some((c) => c.path === '/approvals' && c.query.has('job_id'))).toBe(false)
      expect(jobs.detailApprovals).toBeNull()
    })

    it('marks a job that does not exist as missing', async () => {
      const { jobs } = await setup()
      await jobs.openJob('nope')
      expect(jobs.detailMissing).toBe(true)
      expect(jobs.detail).toBeNull()
      expect(jobs.detailError).toBeNull()
    })

    it('drops the answer for a job that is no longer shown', async () => {
      let release: (() => void) | undefined
      const st = jobsState({ jobs: [jobOf('j1'), jobOf('j2')] })
      const { jobs } = await setup(st, (c) => {
        if (c.path === '/jobs/j1') {
          return new Promise<Response>((resolve) => {
            release = () => resolve(json({ ...jobOf('j1'), steps: [], answer: '' }))
          })
        }
        return undefined
      })
      const first = jobs.openJob('j1')
      await flushPromises()
      await jobs.openJob('j2')
      release!()
      await first
      expect(jobs.detail?.id).toBe('j2')
    })

    it('refreshes an open live job on a jobs change and an ended one only on a full refresh', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
      const { jobs, server, events } = await setup(st)
      await jobs.openJob('j1')
      st.jobs[0] = { ...st.jobs[0], state: 'succeeded' }
      st.steps.j1 = [stepOf(1)]
      await events().onChanged(['jobs'])
      await flushPromises()
      expect(jobs.detail?.state).toBe('succeeded')
      expect(jobs.detail?.steps).toHaveLength(1)
      const reads = () => server.calls.filter((c) => c.path === '/jobs/j1').length
      const n = reads()
      await events().onChanged(['jobs'])
      await flushPromises()
      expect(reads()).toBe(n)
      await events().onReady()
      await flushPromises()
      expect(reads()).toBe(n + 1)
    })
  })

  describe('cancel', () => {
    it('cancels a live job after a confirmation and shows its new state', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
      const { jobs, platform, server } = await setup(st)
      await jobs.refresh()
      await jobs.openJob('j1')
      expect(await jobs.cancel('j1')).toBe(true)
      expect(platform.confirm).toHaveBeenCalledWith(
        expect.objectContaining({ title: 'Cancel this task?', confirmText: 'Cancel task', danger: true })
      )
      expect(server.count('POST', '/jobs/j1/cancel')).toBe(1)
      expect(server.calls.find((c) => c.path === '/jobs/j1/cancel')!.body).toEqual({})
      expect(jobs.jobs[0].state).toBe('cancelled')
      expect(jobs.detail?.state).toBe('cancelled')
      expect(jobs.actionError).toBeNull()
    })

    it('reads the list again after a cancel under a state filter', async () => {
      const st = jobsState({ jobs: [jobOf('j2', { state: 'running' }), jobOf('j1', { state: 'running' })] })
      const { jobs } = await setup(st)
      await jobs.setFilter('running')
      expect(ids(jobs.jobs)).toEqual(['j2', 'j1'])
      expect(await jobs.cancel('j1')).toBe(true)
      expect(ids(jobs.jobs)).toEqual(['j2']) // no longer in the filtered state
    })

    it('does nothing when the user does not confirm', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'queued' })] })
      const { jobs, platform, server } = await setup(st)
      platform.confirm.mockResolvedValueOnce(false)
      expect(await jobs.cancel('j1')).toBe(false)
      expect(server.count('POST', '/jobs/j1/cancel')).toBe(0)
    })

    it('reads the job again when it ended meanwhile (409)', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
      const { jobs } = await setup(st)
      await jobs.refresh()
      await jobs.openJob('j1')
      st.jobs[0] = { ...st.jobs[0], state: 'succeeded' }
      expect(await jobs.cancel('j1')).toBe(false)
      expect(jobs.actionError).toBe('This task has already ended, so it cannot be cancelled.')
      expect(jobs.detail?.state).toBe('succeeded')
      expect(jobs.jobs[0].state).toBe('succeeded')
    })

    it('drops a job that is gone (404)', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
      const { jobs } = await setup(st)
      await jobs.refresh()
      await jobs.openJob('j1')
      st.jobs = []
      expect(await jobs.cancel('j1')).toBe(false)
      expect(jobs.actionError).toBe('This task no longer exists.')
      expect(jobs.detailMissing).toBe(true)
      expect(jobs.jobs).toEqual([])
    })

    it('shows the fixed text of another refusal', async () => {
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })], cancel: apiErr(429, 'rate_limited') })
      const { jobs } = await setup(st)
      await jobs.openJob('j1')
      expect(await jobs.cancel('j1')).toBe(false)
      expect(jobs.actionError).toBe('Too many requests. Wait a moment and try again.')
      expect(jobs.detail?.state).toBe('running')
    })

    it('does not cancel twice at the same time', async () => {
      let release: (() => void) | undefined
      const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
      const { jobs, server } = await setup(st, (c) => {
        if (c.path === '/jobs/j1/cancel') {
          return new Promise<Response>((resolve) => {
            release = () => resolve(json(jobOf('j1', { state: 'cancelled' })))
          })
        }
        return undefined
      })
      const a = jobs.cancel('j1')
      await flushPromises()
      expect(jobs.cancelling).toBe(true)
      expect(await jobs.cancel('j1')).toBe(false)
      release!()
      expect(await a).toBe(true)
      expect(server.count('POST', '/jobs/j1/cancel')).toBe(1)
      expect(jobs.cancelling).toBe(false)
    })
  })

  it('forgets everything when the session ends', async () => {
    const { jobs, store } = await setup(jobsState({ jobs: [jobOf('j1')] }))
    await jobs.setFilter('succeeded')
    await jobs.openJob('j1')
    store.stop()
    await flushPromises()
    expect(jobs.jobs).toEqual([])
    expect(jobs.detail).toBeNull()
    expect(jobs.filter).toBe('')
    expect(jobs.loaded).toBe(false)
  })
})
