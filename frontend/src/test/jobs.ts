import type { Approval, Job, JobDetail, Step } from '../api/types'
import { apiErr, json } from './fixtures'
import type { Call, Handler } from './server'

// Fixtures and a fake jobs backend for tests of the tasks store and screen.

const ENDED = ['succeeded', 'failed', 'cancelled']

// jobOf is a job; an ended one (the default: succeeded) ended at its last
// update unless ended_at is given.
export const jobOf = (id: string, over: Partial<Job> = {}): Job => {
  const j: Job = {
    id,
    trigger_kind: 'chat',
    trigger_ref: '',
    goal: 'goal of ' + id,
    state: 'succeeded',
    attempt: 1,
    runs: 1,
    created_at: '2026-01-01T10:00:00Z',
    updated_at: '2026-01-01T10:05:00Z',
    ended_at: null,
    error: '',
    steps_count: 0,
    tokens_in: 0,
    tokens_out: 0,
    partial: '',
    partial_state: '',
    ...over
  }
  if (!('ended_at' in over) && ENDED.includes(j.state)) j.ended_at = j.updated_at
  return j
}

export const stepOf = (n: number, over: Partial<Step> = {}): Step => ({
  n,
  kind: 'llm',
  tool: '',
  call_id: '',
  status: 'ok',
  tokens_in: 0,
  tokens_out: 0,
  summary: '',
  approval_id: '',
  created_at: '2026-01-01T10:01:00Z',
  ...over
})

export interface JobsState {
  jobs: Job[] // newest first
  steps: Record<string, Step[]>
  answers: Record<string, string>
  approvals: Record<string, Approval[]> // pending approvals per job
  cancel?: Response // the response of every cancel while set
}

export const jobsState = (over: Partial<JobsState> = {}): JobsState => ({
  jobs: [],
  steps: {},
  answers: {},
  approvals: {},
  ...over
})

// jobsHandler answers the jobs routes from the state, like the server:
// newest first, the id of the last entry as the cursor, state filtering.
export const jobsHandler =
  (st: JobsState): Handler =>
  (c: Call) => {
    if (c.method === 'GET' && c.path === '/jobs') {
      const state = c.query.get('state')
      const list = state ? st.jobs.filter((j) => j.state === state) : st.jobs
      const limit = Number(c.query.get('limit') ?? '50')
      const cursor = c.query.get('cursor')
      let start = 0
      if (cursor !== null) {
        const i = list.findIndex((x) => x.id === cursor)
        if (i < 0) return apiErr(400, 'invalid_request')
        start = i + 1
      }
      const items = list.slice(start, start + limit)
      const more = start + limit < list.length
      return json({ jobs: items, next_cursor: more ? items[items.length - 1].id : null })
    }
    const one = /^\/jobs\/([^/]+)$/.exec(c.path)
    if (one && c.method === 'GET') {
      const j = st.jobs.find((x) => x.id === decodeURIComponent(one[1]))
      if (!j) return apiErr(404, 'not_found')
      const d: JobDetail = { ...j, steps: st.steps[j.id] ?? [], answer: st.answers[j.id] ?? '' }
      return json(d)
    }
    const cancel = /^\/jobs\/([^/]+)\/cancel$/.exec(c.path)
    if (cancel && c.method === 'POST') {
      if (st.cancel) return st.cancel.clone()
      const i = st.jobs.findIndex((x) => x.id === decodeURIComponent(cancel[1]))
      if (i < 0) return apiErr(404, 'not_found')
      if (ENDED.includes(st.jobs[i].state)) return apiErr(409, 'conflict')
      st.jobs[i] = { ...st.jobs[i], state: 'cancelled', updated_at: '2026-01-01T12:00:00Z', ended_at: '2026-01-01T12:00:00Z' }
      return json(st.jobs[i])
    }
    if (c.method === 'GET' && c.path === '/approvals' && c.query.has('job_id')) {
      return json({ approvals: st.approvals[c.query.get('job_id')!] ?? [], next_cursor: null })
    }
    return undefined
  }
