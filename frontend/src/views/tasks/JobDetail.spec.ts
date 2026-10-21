import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import JobDetail from './JobDetail.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, approvalOf } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import { jobOf, jobsHandler, jobsState, stepOf, type JobsState } from '../../test/jobs'
import { useJobsStore } from '../../stores/jobs'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: JobsState, id = 'j1', extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? jobsHandler(st)(c))
  const agent = await startStore(server)
  const jobs = useJobsStore()
  await jobs.openJob(id)
  wrapper = mount(JobDetail, { global: { provide: { [PLATFORM_KEY as symbol]: agent.platform } } })
  await flushPromises()
  return { w: wrapper, server, jobs, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('JobDetail', () => {
  it('shows the goal, state, trigger, times, attempts and token usage', async () => {
    const st = jobsState({
      jobs: [
        jobOf('j1', {
          state: 'succeeded',
          runs: 2,
          goal: 'Sort my notes',
          tokens_in: 2000,
          tokens_out: 100,
          updated_at: '2026-01-01T11:00:00Z',
          ended_at: '2026-01-01T10:30:00Z'
        })
      ],
      // the job's own totals count, not a sum over the steps read
      steps: { j1: [stepOf(1, { tokens_in: 1, tokens_out: 1 })] },
      answers: { j1: 'All sorted' }
    })
    const { w } = await setup(st)
    expect(w.find('.agent-job-detail-goal').text()).toBe('Sort my notes')
    expect(w.find('.agent-job-detail-state').text()).toBe('Done')
    expect(w.find('.agent-job-detail-trigger').text()).toBe('Chat question')
    expect(w.find('.agent-job-detail-created time').attributes('datetime')).toBe('2026-01-01T10:00:00Z')
    expect(w.find('.agent-job-detail-changed').text()).toContain('Ended')
    expect(w.find('.agent-job-detail-ended time').attributes('datetime')).toBe('2026-01-01T10:30:00Z')
    expect(w.find('.agent-job-detail-runs').text()).toBe('2')
    expect(w.find('.agent-job-detail-tokens').text()).toBe('2000 in, 100 out')
    expect(w.find('.agent-job-detail-answer').text()).toBe('All sorted')
    expect(button(w, 'Cancel task')).toBeUndefined()
  })

  it('links a chat job back to its conversation', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1', { trigger_kind: 'chat', trigger_ref: 'conv-1' })] }))
    const link = w.find('.agent-job-detail-conversation')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-chat-conversation', params: { id: 'conv-1' } })
    expect(link.text()).toBe('Open the conversation')
  })

  it.each([
    ['schedule', 'Scheduled review'],
    ['file_event', 'File change'],
    ['manual', 'Review on request'],
    ['later_kind', 'later_kind']
  ])('names the trigger %s', async (kind, label) => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1', { trigger_kind: kind })] }))
    expect(w.find('.agent-job-detail-trigger').text()).toBe(label)
  })

  it('has no conversation link for other jobs', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1', { trigger_kind: 'proactive', trigger_ref: 'x' })] }))
    expect(w.find('.agent-job-detail-conversation').exists()).toBe(false)
  })

  it('links back to the list', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1')] }))
    const back = w.find('.agent-job-detail-back')
    expect(JSON.parse(back.attributes('data-to')!)).toEqual({ name: 'agent-tasks' })
  })

  it('renders steps with hostile summaries, tools and statuses as text', async () => {
    const st = jobsState({
      jobs: [jobOf('j1', { steps_count: 3 })],
      steps: {
        j1: [
          stepOf(1, { kind: 'llm', tokens_in: 5, tokens_out: 7 }),
          stepOf(2, {
            kind: 'tool',
            tool: 'files.read',
            status: '<script>x</script>',
            summary: 'read <a href="javascript:alert(1)">x</a>⁦\u0007' + 'y'.repeat(3000)
          }),
          stepOf(3, { kind: '<u>odd</u>', tool: 'files.write', approval_id: 'a9' })
        ]
      }
    })
    const { w } = await setup(st)
    const steps = w.findAll('.agent-step')
    expect(steps).toHaveLength(3)
    expect(steps[0].find('.agent-step-kind').text()).toBe('Model turn')
    expect(steps[0].find('.agent-step-tokens').text()).toBe('5 in, 7 out')
    expect(steps[1].find('.agent-step-kind').text()).toBe('Tool call')
    expect(steps[1].find('.agent-step-tool').text()).toBe('files.read')
    expect(steps[1].find('.agent-step-status').text()).toBe('<script>x</script>')
    expect(steps[1].find('.agent-step-summary').text()).toContain('read <a href="javascript:alert(1)">x</a>')
    expect(steps[1].findAll('.agent-step-summary .agent-ctl').map((c) => c.text())).toEqual(['\\u{2066}', '\\u{0007}'])
    expect(steps[2].find('.agent-step-kind').text()).toBe('<u>odd</u>')
    expect(w.find('script').exists()).toBe(false)
    expect(w.find('.agent-step a[href^="javascript"]').exists()).toBe(false)
    expect(w.find('u').exists()).toBe(false)
    const link = steps[2].find('.agent-step-approval')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-approvals', query: { job: 'j1' } })
  })

  it('says when there are no steps yet', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1', { state: 'queued' })] }))
    expect(w.find('.agent-job-detail-nosteps').text()).toBe('No steps yet.')
  })

  it('lists the pending approvals of a waiting job with a link to the inbox', async () => {
    const st = jobsState({
      jobs: [jobOf('j1', { state: 'waiting_approval' })],
      approvals: { j1: [approvalOf('a1', { job_id: 'j1', tool: 'files.delete', risk: 'destructive' })] }
    })
    const { w } = await setup(st)
    const list = w.find('.agent-job-detail-approvals')
    expect(list.find('.agent-job-detail-tool').text()).toBe('files.delete')
    const link = list.find('.agent-job-detail-inbox')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-approvals', query: { job: 'j1' } })
  })

  it('shows a known and an unknown error', async () => {
    const st = jobsState({
      jobs: [jobOf('j1', { state: 'failed', error: 'The agent stopped because of an internal error.' })]
    })
    const { w } = await setup(st)
    expect(w.find('.agent-job-detail-error').text()).toBe('The agent stopped because of an internal error.')
  })

  it('cancels a live job after a confirmation', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
    const { w, server, platform } = await setup(st)
    await button(w, 'Cancel task')!.trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledTimes(1)
    expect(server.count('POST', '/jobs/j1/cancel')).toBe(1)
    expect(w.find('.agent-job-detail-state').text()).toBe('Cancelled')
    expect(button(w, 'Cancel task')).toBeUndefined()
  })

  it('shows the race when the job ended meanwhile', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
    const { w } = await setup(st)
    st.jobs[0] = { ...st.jobs[0], state: 'succeeded' }
    await button(w, 'Cancel task')!.trigger('click')
    await flushPromises()
    expect(w.find('[role=alert]').text()).toBe('This task has already ended, so it cannot be cancelled.')
    expect(w.find('.agent-job-detail-state').text()).toBe('Done')
    expect(button(w, 'Cancel task')).toBeUndefined()
  })

  it('shows a job that is gone', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
    const { w } = await setup(st)
    st.jobs = []
    await button(w, 'Cancel task')!.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('This task no longer exists.')
    expect(w.find('.agent-job-detail-goal').exists()).toBe(false)
  })

  it('shows a missing job', async () => {
    const { w } = await setup(jobsState(), 'nope')
    expect(w.text()).toContain('This task no longer exists.')
  })

  it('shows a load error with a retry', async () => {
    let fail = true
    const st = jobsState({ jobs: [jobOf('j1')] })
    const { w } = await setup(st, 'j1', (c) => (fail && c.path === '/jobs/j1' ? apiErr(503, 'unavailable') : undefined))
    expect(w.find('[role=alert] .agent-state-title').text()).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await button(w, 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-job-detail-goal').text()).toBe('goal of j1')
  })

  it('shows the partial answer of a running job as unfinished plain text', async () => {
    const hostile = '<b>so far</b>\u202e'
    const job = jobOf('j1', { state: 'running', partial: hostile, partial_state: 'writing' })
    const { w } = await setup(jobsState({ jobs: [job] }))
    const section = w.find('.agent-job-detail-partial-section')
    expect(section.find('h3').text()).toBe('Partial answer (unfinished)')
    const text = section.find('.agent-job-detail-partial')
    expect(text.find('b').exists()).toBe(false)
    expect(text.text()).toBe('<b>so far</b>\\u{202E}')
    expect(w.find('.agent-job-detail-answer-section').exists()).toBe(false)
  })

  it('says the model of a running job is still thinking', async () => {
    const job = jobOf('j1', { state: 'running', partial: '', partial_state: 'thinking' })
    const { w } = await setup(jobsState({ jobs: [job] }))
    expect(w.find('.agent-job-detail-partial-section').text()).toContain('Thinking…')
  })

  it.each(['waiting_approval', 'failed', 'succeeded', 'cancelled', 'queued'])(
    'shows no partial answer for a %s job',
    async (state) => {
      const job = jobOf('j1', { state, partial: 'stale', partial_state: 'writing', error: 'The agent stopped.' })
      const { w } = await setup(jobsState({ jobs: [job] }))
      expect(w.find('.agent-job-detail-partial-section').exists()).toBe(false)
      expect(w.text()).not.toContain('stale')
    }
  )
})

describe('JobDetail during an outage', () => {
  it('keeps the task and shows no inline error while the service is down', async () => {
    let fail = false
    const { w, jobs, store } = await setup(jobsState({ jobs: [jobOf('j1')] }), 'j1', (c) =>
      fail && c.path === '/jobs/j1' ? apiErr(503, 'unavailable') : undefined
    )
    fail = true
    await jobs.refreshDetail()
    await flushPromises()
    expect(store.serviceDown).toBe(true)
    expect(w.find('[role=alert]').exists()).toBe(false)
    expect(w.find('.agent-job-detail-goal').text()).toBe('goal of j1')
  })

  it('shows exactly one error card with no data', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1')] }), 'j1', (c) =>
      c.path === '/jobs/j1' ? apiErr(503, 'unavailable') : undefined
    )
    expect(w.findAll('[role=alert]')).toHaveLength(1)
    expect(w.find('[role=alert] [data-stub=OcIcon]').attributes('data-icon')).toBe('wifi-off')
  })

  it('lays out the main column and the facts card, with chips for state and trigger', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'failed' })], steps: { j1: [stepOf(1)] } })
    const { w } = await setup(st)
    const main = w.find('.agent-job-detail-main')
    const side = w.find('aside.agent-job-detail-side')
    expect(main.exists()).toBe(true)
    expect(side.classes()).toContain('agent-surface')
    expect(main.find('.agent-job-detail-steps').exists()).toBe(true)
    expect(side.find('.agent-job-detail-facts').exists()).toBe(true)
    expect(side.find('.agent-job-detail-state').classes()).toEqual(expect.arrayContaining(['agent-chip', 'agent-chip--danger']))
    expect(side.find('.agent-job-detail-trigger').classes()).toContain('agent-chip--neutral')
  })
})
