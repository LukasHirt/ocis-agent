import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Approvals from './Approvals.vue'
import { PLATFORM_KEY } from '../platform'
import { apiErr, approvalOf, json } from '../test/fixtures'
import { fakeServer, startStore, type Call, type Handler } from '../test/server'
import type { Approval } from '../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const jobOf = (id: string, goal: string) => ({
  id,
  trigger_kind: 'chat',
  goal,
  state: 'waiting_approval',
  attempt: 1,
  runs: 1,
  created_at: null,
  updated_at: null,
  error: '',
  steps_count: 0,
  steps: [],
  answer: ''
})

// serve answers GET /approvals from the given lists (pending without a
// state, unknown for state=unknown) and GET /jobs/{id} with goals.
const serve = (
  lists: { pending?: Approval[]; unknown?: Approval[] },
  goals: Record<string, string> = {},
  extra?: Handler
): Handler => {
  return (c: Call) => {
    const r = extra?.(c)
    if (r) return r
    if (c.method === 'GET' && c.path === '/approvals' && !c.query.has('cursor')) {
      const state = c.query.get('state')
      if (state === null) return json({ approvals: lists.pending ?? [], next_cursor: null })
      if (state === 'unknown') return json({ approvals: lists.unknown ?? [], next_cursor: null })
    }
    const m = /^\/jobs\/([^/]+)$/.exec(c.path)
    if (c.method === 'GET' && m) {
      const id = decodeURIComponent(m[1])
      return id in goals ? json(jobOf(id, goals[id])) : apiErr(404, 'not_found')
    }
    return undefined
  }
}

const setup = async (handler: Handler) => {
  const server = fakeServer(handler)
  const { store, platform } = await startStore(server)
  wrapper = mount(Approvals, { global: { provide: { [PLATFORM_KEY as symbol]: platform } } })
  await flushPromises()
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('Approvals', () => {
  it('shows the empty state and the count in the title', async () => {
    const { w } = await setup(serve({}))
    expect(w.find('h1').text()).toBe('Approvals (0)')
    expect(w.text()).toContain('Nothing needs your approval')
  })

  it('counts only approvals that can still be decided in the title', async () => {
    const { w } = await setup(
      serve({ pending: [approvalOf('a1'), approvalOf('a2'), approvalOf('a3', { job_ended: true })] })
    )
    expect(w.find('h1').text()).toBe('Approvals (2)')
    expect(w.text()).not.toContain('Nothing needs your approval')
  })

  it('shows a spinner while the approvals load', async () => {
    let release!: (r: Response) => void
    const { w } = await setup(
      serve({}, {}, (c) =>
        c.path === '/approvals' && !c.query.has('state')
          ? new Promise<Response>((r) => (release = r))
          : undefined
      )
    )
    expect(w.find('[data-stub=OcSpinner]').exists()).toBe(true)
    expect(w.text()).not.toContain('Nothing needs your approval')
    release(json({ approvals: [], next_cursor: null }))
    await flushPromises()
    expect(w.find('[data-stub=OcSpinner]').exists()).toBe(false)
    expect(w.text()).toContain('Nothing needs your approval')
  })

  it('shows the fixed error with a retry when the approvals cannot be loaded', async () => {
    let fail = true
    const { w } = await setup(
      serve({ pending: [approvalOf('a1')] }, {}, (c) =>
        fail && c.path === '/approvals' ? apiErr(503, 'unavailable') : undefined
      )
    )
    expect(w.find('[role=alert]').text()).toBe('The agent service is not reachable right now. Try again later.')
    expect(w.text()).not.toContain('Nothing needs your approval')
    fail = false
    await button(w, 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.find('[role=alert]').exists()).toBe(false)
    expect(w.findAll('.agent-card')).toHaveLength(1)
  })

  it('groups pending approvals by job, newest group first, with the goal as text', async () => {
    const { w, server } = await setup(
      serve(
        {
          pending: [
            approvalOf('b2', { job_id: 'jb' }),
            approvalOf('a1', { job_id: 'ja' }),
            approvalOf('b1', { job_id: 'jb' })
          ]
        },
        { jb: '<img src=x onerror=1> tidy ‮ up', ja: 'save the report' }
      )
    )
    const groups = w.findAll('.agent-group')
    expect(groups).toHaveLength(2)
    expect(groups[0].findAll('.agent-card')).toHaveLength(2)
    expect(groups[0].find('.agent-group-goal').text()).toContain('<img src=x onerror=1> tidy \\u{202E} up')
    expect(groups[0].find('img').exists()).toBe(false)
    expect(groups[0].text()).toContain('2 approvals')
    expect(groups[1].find('.agent-group-goal').text()).toContain('save the report')
    expect(groups[1].text()).toContain('1 approval')
    expect(server.count('GET', '/jobs/jb')).toBe(1)
    expect(server.count('GET', '/jobs/ja')).toBe(1)
  })

  it('tolerates a failed goal lookup', async () => {
    const { w } = await setup(serve({ pending: [approvalOf('a1', { job_id: 'job-12345678-x' })] }))
    const g = w.find('.agent-group')
    expect(g.find('.agent-group-goal').exists()).toBe(false)
    expect(g.find('.agent-group-title').text()).toContain('job-1234')
    expect(g.findAll('.agent-card')).toHaveLength(1)
  })

  it('bulk approves a group with every displayed args_hash', async () => {
    const items = [approvalOf('a1', { args_hash: 'h1' }), approvalOf('a2', { args_hash: 'h2' })]
    const { w, server, platform } = await setup(
      serve({ pending: items }, {}, (c) =>
        c.path === '/approvals/bulk-decision' ? json({ approvals: items }) : undefined
      )
    )
    await button(w, 'Approve all 2')!.trigger('click')
    await flushPromises()
    const bulk = server.calls.filter((c) => c.path === '/approvals/bulk-decision')
    expect(bulk).toHaveLength(1)
    expect(bulk[0].body).toEqual({
      decision: 'approve',
      items: [
        { id: 'a1', args_hash: 'h1' },
        { id: 'a2', args_hash: 'h2' }
      ]
    })
    expect(platform.confirm).not.toHaveBeenCalled()
    expect(platform.notify).toHaveBeenCalledWith('success', 'Approved 2 actions')
  })

  it('bulk rejects a group without confirmation', async () => {
    const items = [approvalOf('a1', { risk: 'destructive' }), approvalOf('a2')]
    const { w, server, platform } = await setup(
      serve({ pending: items }, {}, (c) =>
        c.path === '/approvals/bulk-decision' ? json({ approvals: items }) : undefined
      )
    )
    await button(w, 'Reject all 2')!.trigger('click')
    await flushPromises()
    expect(platform.confirm).not.toHaveBeenCalled()
    const bulk = server.calls.filter((c) => c.path === '/approvals/bulk-decision')
    expect(bulk[0].body).toEqual({
      decision: 'reject',
      items: [
        { id: 'a1', args_hash: 'h-a1' },
        { id: 'a2', args_hash: 'h-a2' }
      ]
    })
  })

  it('confirms a bulk approval that contains a dangerous action; cancel sends nothing', async () => {
    const items = [
      approvalOf('a1', { tool: 'files.delete', risk: 'destructive', preview: { paths: ['/old.txt'] } }),
      approvalOf('a2')
    ]
    const { w, server, platform } = await setup(serve({ pending: items }))
    platform.confirm.mockResolvedValueOnce(false)
    await button(w, 'Approve all 2')!.trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledTimes(1)
    const opts = platform.confirm.mock.calls[0][0]
    expect(opts.danger).toBe(true)
    expect(opts.message).toContain('files.delete')
    expect(opts.message).toContain('/old.txt')
    expect(server.count('POST', '/approvals/bulk-decision')).toBe(0)
  })

  it('keeps the whole group when a bulk decision has stale arguments', async () => {
    const items = [approvalOf('a1'), approvalOf('a2')]
    const { w, server, platform } = await setup(
      serve({ pending: items }, {}, (c) =>
        c.path === '/approvals/bulk-decision' ? apiErr(409, 'stale_arguments') : undefined
      )
    )
    const before = server.count('GET', '/approvals')
    await button(w, 'Approve all 2')!.trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith('error', 'The action changed; review the updated request')
    expect(server.count('GET', '/approvals')).toBeGreaterThan(before)
    expect(w.findAll('.agent-card')).toHaveLength(2)
    expect(button(w, 'Approve all 2')!.attributes('disabled')).toBeUndefined()
  })

  it('locks the group cards while a bulk decision is in flight', async () => {
    let release!: (r: Response) => void
    const items = [approvalOf('a1'), approvalOf('a2')]
    const { w, server } = await setup(
      serve({ pending: items }, {}, (c) =>
        c.path === '/approvals/bulk-decision' ? new Promise<Response>((r) => (release = r)) : undefined
      )
    )
    await button(w, 'Approve all 2')!.trigger('click')
    await button(w, 'Approve all 2')!.trigger('click')
    await flushPromises()
    expect(server.count('POST', '/approvals/bulk-decision')).toBe(1)
    const decide = w.findAll('.agent-card button').filter((b) => ['Approve', 'Reject'].includes(b.text()))
    expect(decide).toHaveLength(4)
    for (const b of decide) expect(b.attributes('disabled')).toBeDefined()
    release(json({ approvals: items }))
    await flushPromises()
  })

  it('offers no bulk decision for a single approval or for ended jobs', async () => {
    const { w } = await setup(
      serve({
        pending: [
          approvalOf('a1'),
          approvalOf('e1', { job_id: 'je', job_ended: true }),
          approvalOf('e2', { job_id: 'je', job_ended: true })
        ]
      })
    )
    expect(w.findAll('.agent-group')).toHaveLength(2)
    expect(w.findAll('button').some((b) => b.text().startsWith('Approve all'))).toBe(false)
  })

  it('disables the bulk decision above the server limit', async () => {
    const many = Array.from({ length: 101 }, (_, i) => approvalOf('m' + i))
    const { w } = await setup(serve({ pending: many }))
    expect(button(w, 'Approve all 101')!.attributes('disabled')).toBeDefined()
    expect(button(w, 'Reject all 101')!.attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('decide them one by one')
  })

  it('shows actions with an unknown outcome at the top for verification', async () => {
    const { w } = await setup(
      serve({
        pending: [approvalOf('a1')],
        unknown: [
          approvalOf('u1', {
            state: 'unknown',
            needs_verification: true,
            args: { path: '/Docs/x.txt' },
            outcome: { state: 'unknown', summary: '', reason: 'No outcome was recorded.', diff: '' }
          })
        ]
      })
    )
    const sections = w.findAll('section > section')
    expect(sections[0].classes()).toContain('agent-verify')
    const verify = w.find('.agent-verify')
    expect(verify.find('h2').text()).toBe('Outcome unknown — please verify')
    expect(verify.text()).toContain('/Docs/x.txt')
    expect(verify.findAll('button').filter((b) => b.text() === 'Approve')).toHaveLength(0)
  })

  it('loads the history lazily and pages it with the cursor', async () => {
    const h = (id: string, state: string) =>
      approvalOf(id, { state, outcome: { state: state === 'executed' ? 'done' : state, summary: '', reason: 'r-' + id, diff: '' } })
    const { w, server } = await setup(
      serve({ pending: [approvalOf('p1')] }, {}, (c) => {
        if (c.path !== '/approvals' || c.query.get('state') !== 'all') return undefined
        if (!c.query.has('cursor'))
          return json({ approvals: [approvalOf('p1'), h('h1', 'executed'), h('h2', 'rejected')], next_cursor: 'h2' })
        if (c.query.get('cursor') === 'h2') return json({ approvals: [h('h3', 'expired')], next_cursor: null })
        return undefined
      })
    )
    const all = () => server.calls.filter((c) => c.path === '/approvals' && c.query.get('state') === 'all')
    expect(all()).toHaveLength(0)
    const toggle = button(w, 'History')!
    expect(toggle.attributes('aria-expanded')).toBe('false')
    await toggle.trigger('click')
    await flushPromises()
    expect(button(w, 'History')!.attributes('aria-expanded')).toBe('true')
    const history = () => w.find('.agent-history')
    expect(history().findAll('.agent-card')).toHaveLength(2)
    expect(history().text()).toContain('r-h1')
    await button(w, 'Load more')!.trigger('click')
    await flushPromises()
    expect(all()[1].query.get('cursor')).toBe('h2')
    expect(history().findAll('.agent-card')).toHaveLength(3)
    expect(history().text()).toContain('r-h3')
    expect(button(w, 'Load more')).toBeUndefined()
    await button(w, 'History')!.trigger('click')
    await button(w, 'History')!.trigger('click')
    await flushPromises()
    expect(all()).toHaveLength(2)
  })

  it('shows a history error with a retry', async () => {
    let fail = true
    const { w } = await setup(
      serve({}, {}, (c) =>
        c.query.get('state') === 'all'
          ? fail
            ? apiErr(500, 'internal')
            : json({ approvals: [approvalOf('h1', { state: 'rejected' })], next_cursor: null })
          : undefined
      )
    )
    await button(w, 'History')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-history [role=alert]').text()).toBe('Something went wrong in the agent service.')
    fail = false
    await w.find('.agent-history').findAll('button').find((b) => b.text() === 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-history').findAll('.agent-card')).toHaveLength(1)
  })

  it('removes a decided card from the pending list', async () => {
    let pending = [approvalOf('a1'), approvalOf('a2', { job_id: 'j2' })]
    const { w } = await setup((c) => {
      if (c.path === '/approvals/a1/decision') {
        pending = pending.filter((a) => a.id !== 'a1')
        return json({ ...approvalOf('a1'), state: 'approved' })
      }
      if (c.path === '/approvals' && !c.query.has('state')) return json({ approvals: pending, next_cursor: null })
      return undefined
    })
    expect(w.findAll('.agent-card')).toHaveLength(2)
    await w.findAll('button').find((b) => b.text() === 'Approve')!.trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-card')).toHaveLength(1)
    expect(w.find('h1').text()).toBe('Approvals (1)')
  })

  it('says when more approvals exist than are shown', async () => {
    const { w, store } = await setup(serve({ pending: [approvalOf('a1')] }))
    store.approvals = { ...store.approvals, complete: false }
    await flushPromises()
    expect(w.text()).toContain('More approvals are waiting than are shown here')
  })
})
