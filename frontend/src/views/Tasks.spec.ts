import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Tasks from './Tasks.vue'
import { PLATFORM_KEY } from '../platform'
import { apiErr, json, statusOf } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import { jobOf, jobsHandler, jobsState, type JobsState } from '../test/jobs'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: JobsState, props: { id?: string } = {}, extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? jobsHandler(st)(c))
  const agent = await startStore(server)
  wrapper = mount(Tasks, {
    props,
    global: { provide: { [PLATFORM_KEY as symbol]: agent.platform }, mocks: { $router: { push: vi.fn() } } }
  })
  await flushPromises()
  return { w: wrapper, server, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)
const goals = (w: VueWrapper) => w.findAll('.agent-job-goal').map((g) => g.text())

describe('Tasks', () => {
  it('lists the jobs newest first', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j2'), jobOf('j1')] }))
    expect(w.find('h1').text()).toBe('Tasks')
    expect(goals(w)).toEqual(['goal of j2', 'goal of j1'])
  })

  it('says when there are no tasks', async () => {
    const { w } = await setup(jobsState())
    expect(w.find('.agent-tasks-empty').text()).toBe('No tasks yet.')
  })

  it('filters by state', async () => {
    const st = jobsState({ jobs: [jobOf('j2', { state: 'failed' }), jobOf('j1', { state: 'running' })] })
    const { w, server } = await setup(st)
    const select = w.find('[data-testid="agent-tasks-filter"] select')
    const labels = select.findAll('option').map((o) => o.text())
    expect(labels).toEqual(['All tasks', 'Waiting', 'Working', 'Needs your approval', 'Done', 'Failed', 'Cancelled'])
    await select.setValue(String(labels.indexOf('Failed')))
    await flushPromises()
    expect(goals(w)).toEqual(['goal of j2'])
    expect(server.calls.filter((c) => c.path === '/jobs').at(-1)!.query.get('state')).toBe('failed')
    expect(w.find('.agent-tasks-empty').exists()).toBe(false)
    await select.setValue(String(labels.indexOf('Cancelled')))
    await flushPromises()
    expect(w.find('.agent-tasks-empty').text()).toBe('No tasks in this state.')
  })

  it('loads more while a cursor is given, also after an empty page', async () => {
    let n = 0
    const { w } = await setup(jobsState(), {}, (c) => {
      if (c.path !== '/jobs') return undefined
      n++
      if (n === 1) return json({ jobs: [jobOf('j3')], next_cursor: 'j3' })
      if (n === 2) return json({ jobs: [], next_cursor: 'j3' })
      return json({ jobs: [jobOf('j1')], next_cursor: null })
    })
    expect(goals(w)).toEqual(['goal of j3'])
    await button(w, 'Load more')!.trigger('click')
    await flushPromises()
    expect(goals(w)).toEqual(['goal of j3'])
    await button(w, 'Load more')!.trigger('click')
    await flushPromises()
    expect(goals(w)).toEqual(['goal of j3', 'goal of j1'])
    expect(button(w, 'Load more')).toBeUndefined()
  })

  it('updates the list on a jobs change event', async () => {
    const st = jobsState({ jobs: [jobOf('j1', { state: 'running' })] })
    const { w, events } = await setup(st)
    st.jobs.unshift(jobOf('j2', { state: 'queued' }))
    st.jobs[1] = { ...st.jobs[1], state: 'succeeded' }
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(goals(w)).toEqual(['goal of j2', 'goal of j1'])
    expect(w.findAll('.agent-job-state').map((s) => s.text())).toEqual(['Waiting', 'Done'])
  })

  it('shows the detail of the route id and back the list without it', async () => {
    const st = jobsState({ jobs: [jobOf('j2'), jobOf('j1', { goal: 'the one' })] })
    const { w } = await setup(st, { id: 'j1' })
    expect(w.find('.agent-job-detail-goal').text()).toBe('the one')
    expect(w.find('.agent-tasks-list').exists()).toBe(false)
    await w.setProps({ id: undefined })
    await flushPromises()
    expect(goals(w)).toEqual(['goal of j2', 'the one'])
  })

  it('opens the task again when it is shown in a new session', async () => {
    const st = jobsState({ jobs: [jobOf('j2'), jobOf('j1', { goal: 'the one' })] })
    const { w, store, server, platform } = await setup(st, { id: 'j1' })
    w.unmount()
    wrapper = undefined
    // the app is left and opened again on the same address
    store.stop()
    await startStore(server, platform)
    wrapper = mount(Tasks, {
      props: { id: 'j1' },
      global: { provide: { [PLATFORM_KEY as symbol]: platform }, mocks: { $router: { push: vi.fn() } } }
    })
    await flushPromises()
    expect(wrapper.find('.agent-job-detail-goal').text()).toBe('the one')
  })

  it('stops listening once it is left', async () => {
    const { w, server, events } = await setup(jobsState({ jobs: [jobOf('j1')] }))
    w.unmount()
    wrapper = undefined
    const n = server.calls.length
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(server.calls.filter((c) => c.path === '/jobs').length).toBe(
      server.calls.slice(0, n).filter((c) => c.path === '/jobs').length
    )
  })
})

describe('Tasks: review now', () => {
  const review = (w: VueWrapper) => button(w, 'Review now')
  const runs = (server: { calls: { method: string; path: string }[] }) =>
    server.calls.filter((c) => c.method === 'POST' && c.path === '/proactive/run')

  it('starts a review, says that it only proposes and reads the list again', async () => {
    const st = jobsState()
    const { w, server } = await setup(st, {}, (c) => {
      if (c.method !== 'POST' || c.path !== '/proactive/run') return undefined
      const j = jobOf('jr', { trigger_kind: 'manual', state: 'queued', goal: 'Review my recent file changes' })
      st.jobs.unshift(j)
      return json(j, 202)
    })
    const lists = server.count('GET', '/jobs')
    const statuses = server.count('GET', '/status')
    await review(w)!.trigger('click')
    await flushPromises()
    expect(runs(server)).toHaveLength(1)
    expect(runs(server)[0]).toMatchObject({ body: {} })
    expect(w.find('[data-testid="agent-review-result"]').text()).toBe(
      'A review has started. The agent only proposes actions: nothing changes until you approve it in the approvals inbox.'
    )
    expect(server.count('GET', '/jobs')).toBeGreaterThan(lists)
    expect(server.count('GET', '/status')).toBeGreaterThan(statuses)
    expect(w.findAll('.agent-job-trigger').map((t) => t.text())).toEqual(['Review on request'])
  })

  it.each([
    ['proactive_disabled', 'Proactive work is turned off. Turn it on in Settings first.'],
    ['kill_switch', 'The operator has stopped the agent for now.'],
    ['paused', 'The agent is paused. Resume it first.'],
    ['credentials_invalid', 'The agent has lost access to your files. Reconnect it.'],
    ['live_job', 'A review is already waiting, working or waiting for your approval.'],
    ['daily_cap', "Today's limit of reviews is reached. It resets at midnight UTC."],
    ['budget', "Most of today's token budget is used. Reviews resume at midnight UTC."],
    ['not_enrolled', 'The agent is not set up for you yet. Set it up first.'],
    ['teapot', 'Something went wrong. Try again later.']
  ])('shows a fixed text for 409 %s, never the server text', async (code, text) => {
    const { w } = await setup(jobsState(), {}, (c) =>
      c.method === 'POST' && c.path === '/proactive/run' ? apiErr(409, code) : undefined
    )
    await review(w)!.trigger('click')
    await flushPromises()
    const alert = w.find('[data-testid="agent-review-error"]')
    expect(alert.attributes('role')).toBe('alert')
    expect(alert.text()).toBe(text)
    expect(w.text()).not.toContain('server text')
    expect(w.find('[data-testid="agent-review-result"]').exists()).toBe(false)
  })

  it('says when to try again after the cooldown', async () => {
    const { w } = await setup(jobsState(), {}, (c) =>
      c.method === 'POST' && c.path === '/proactive/run'
        ? new Response(JSON.stringify({ error: { code: 'rate_limited', message: 'server text' } }), {
            status: 429,
            headers: { 'Content-Type': 'application/json', 'Retry-After': '4' }
          })
        : undefined
    )
    await review(w)!.trigger('click')
    await flushPromises()
    expect(w.find('[data-testid="agent-review-error"]').text().replace(/\s+/g, ' ')).toBe(
      'Too many requests. Wait a moment and try again. Try again in 4 seconds.'
    )
  })

  it('sends once on a double click and clears the last result on the next run', async () => {
    let answer: (r: Response) => void = () => {}
    const { w, server } = await setup(jobsState(), {}, (c) =>
      c.method === 'POST' && c.path === '/proactive/run'
        ? new Promise<Response>((r) => (answer = r))
        : undefined
    )
    await review(w)!.trigger('click')
    await review(w)!.trigger('click')
    expect(review(w)!.attributes('disabled')).toBeDefined()
    expect(runs(server)).toHaveLength(1)
    answer(apiErr(409, 'live_job'))
    await flushPromises()
    expect(w.find('[data-testid="agent-review-error"]').exists()).toBe(true)
    await review(w)!.trigger('click')
    expect(w.find('[data-testid="agent-review-error"]').exists()).toBe(false)
    answer(json(jobOf('jr', { trigger_kind: 'manual', state: 'queued' }), 202))
    await flushPromises()
    expect(w.find('[data-testid="agent-review-result"]').exists()).toBe(true)
  })

  it('is not offered on a task detail', async () => {
    const { w } = await setup(jobsState({ jobs: [jobOf('j1')] }), { id: 'j1' })
    expect(review(w)).toBeUndefined()
  })

  it('is disabled until the agent is set up', async () => {
    const { w } = await setup(jobsState(), {}, (c) =>
      c.path === '/status' ? json(statusOf({ enrolled: false })) : undefined
    )
    expect(review(w)!.attributes('disabled')).toBeDefined()
  })
})

describe('Tasks without a session', () => {
  it('shows a spinner instead of a blank screen, for the list and for a task', async () => {
    wrapper = mount(Tasks, { global: { mocks: { $router: { push: vi.fn() } } } })
    await flushPromises()
    expect(wrapper.find('[data-stub=OcSpinner]').exists()).toBe(true)
    expect(button(wrapper, 'Review now')!.attributes('disabled')).toBeDefined()
    wrapper.unmount()
    wrapper = mount(Tasks, { props: { id: 'j1' }, global: { mocks: { $router: { push: vi.fn() } } } })
    await flushPromises()
    expect(wrapper.find('[data-stub=OcSpinner]').exists()).toBe(true)
  })
})
