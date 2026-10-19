import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Tasks from './Tasks.vue'
import { PLATFORM_KEY } from '../platform'
import { json } from '../test/fixtures'
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
