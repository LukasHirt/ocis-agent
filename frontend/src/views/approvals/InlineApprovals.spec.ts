import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import InlineApprovals from './InlineApprovals.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, approvalOf, json } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import type { Approval } from '../../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (items: Approval[], handler?: Handler, extra: Record<string, unknown> = {}) => {
  const server = fakeServer(handler)
  const { platform, store } = await startStore(server)
  wrapper = mount(InlineApprovals, {
    props: { jobId: 'j1', items, more: false, error: null, ...extra },
    global: { provide: { [PLATFORM_KEY as symbol]: platform } },
    attachTo: document.body
  })
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)!
const decisions = (server: ReturnType<typeof fakeServer>) =>
  server.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/decision'))

describe('InlineApprovals', () => {
  it('shows the exact arguments of every pending item without a click', async () => {
    const { w } = await setup([
      approvalOf('a1', { args: { path: '/one.txt' } }),
      approvalOf('a2', { args: { path: '/two.txt' }, risk: 'destructive' })
    ])
    const cards = w.findAll('.agent-card--compact')
    expect(cards).toHaveLength(2)
    for (const [i, p] of ['/one.txt', '/two.txt'].entries()) {
      const args = cards[i].find('.agent-card-args')
      expect(args.text()).toContain(p)
      expect(args.element.closest('details')).toBeNull()
    }
  })

  it('renders hostile text as text', async () => {
    const { w } = await setup([
      approvalOf('a1', {
        tool: '<img src=x onerror=alert(1)>',
        args: { path: '/a‮.txt', note: '<img src=y onerror=alert(2)>' },
        preview: { paths: ['<img src=z>'] },
        rationale: '<img src=r onerror=alert(3)>‮'
      })
    ])
    expect(w.find('img').exists()).toBe(false)
    expect(w.text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.text()).toContain('<img src=r onerror=alert(3)>')
    expect(w.findAll('.agent-ctl').length).toBeGreaterThan(0)
    expect(w.html()).not.toContain('‮')
  })

  it('approves a write with the displayed hash, then shows one calm line and emits settled', async () => {
    const a = approvalOf('a1', { tool: 'files.write_new', args_hash: 'sha256:shown', preview: { paths: ['/x.txt'] } })
    const { w, server, platform } = await setup([a], (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.confirm).not.toHaveBeenCalled()
    expect(decisions(server)[0].body).toEqual({ decision: 'approve', args_hash: 'sha256:shown' })
    expect(w.find('.agent-inline-decided').text()).toBe('Approved: files.write_new "/x.txt"')
    expect(w.find('.agent-card').exists()).toBe(false)
    expect(w.emitted('settled')).toHaveLength(1)
  })

  it('moves the focus to the decided line, then to the next pending card when the line goes', async () => {
    const a1 = approvalOf('a1', { tool: 'files.write_new' })
    const a2 = approvalOf('a2', { tool: 'files.write_new' })
    const { w } = await setup([a1], (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a1, state: 'approved' }) : undefined
    )
    await w.setProps({ items: [a1, a2] })
    const first = w.findAll('.agent-card')[0]
    const approve = first.findAll('button').find((b) => b.text() === 'Approve')!
    ;(approve.element as HTMLElement).focus()
    await approve.trigger('click')
    await flushPromises()
    expect(document.activeElement).toBe(w.find('.agent-inline-decided').element)
    await w.setProps({ items: [a2] })
    await flushPromises()
    expect(w.find('.agent-inline-decided').exists()).toBe(false)
    expect(w.find('.agent-card').element.contains(document.activeElement)).toBe(true)
  })

  it('moves the focus to the composer when the last card is decided and the list goes', async () => {
    const composer = document.createElement('div')
    composer.className = 'agent-composer'
    composer.innerHTML = '<textarea></textarea>'
    document.body.appendChild(composer)
    const a = approvalOf('a1', { tool: 'files.write_new' })
    const { w } = await setup([a], (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    await w.setProps({ items: [] })
    await flushPromises()
    expect(document.activeElement).toBe(composer.querySelector('textarea'))
    composer.remove()
  })

  it('moves the focus to the composer when the component goes while it holds the focus', async () => {
    const composer = document.createElement('div')
    composer.className = 'agent-composer'
    composer.innerHTML = '<textarea></textarea>'
    document.body.appendChild(composer)
    const a = approvalOf('a1', { tool: 'files.write_new' })
    const { w } = await setup([a], (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(document.activeElement).toBe(w.find('.agent-inline-decided').element)
    w.unmount()
    wrapper = undefined
    expect(document.activeElement).toBe(composer.querySelector('textarea'))
    composer.remove()
  })

  it('shows a rejected line after Reject', async () => {
    const a = approvalOf('a1', { risk: 'destructive' })
    const { w } = await setup([a], (c) => (c.path === '/approvals/a1/decision' ? json({ ...a, state: 'rejected' }) : undefined))
    await button(w, 'Reject').trigger('click')
    await flushPromises()
    expect(w.find('.agent-inline-decided').text()).toContain('Rejected: files.write')
  })

  it('sends the hash of the approval as displayed after the props change', async () => {
    const a = approvalOf('a1', { args_hash: 'sha256:old' })
    const { w, server } = await setup([a], (c) =>
      c.path === '/approvals/a1/decision' ? apiErr(409, 'stale_arguments') : undefined
    )
    await w.setProps({ items: [{ ...a, args: { path: '/other' }, args_hash: 'sha256:new' }] })
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(decisions(server)[0].body).toEqual({ decision: 'approve', args_hash: 'sha256:new' })
  })

  it('maps stale_arguments to the fixed toast, no decided line, and still emits settled', async () => {
    const { w, platform } = await setup([approvalOf('a1')], (c) =>
      c.path === '/approvals/a1/decision' ? apiErr(409, 'stale_arguments') : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith('error', 'The action changed; review the updated request')
    expect(w.find('.agent-inline-decided').exists()).toBe(false)
    expect(w.emitted('settled')).toHaveLength(1)
  })

  it('confirms a destructive approval and sends nothing on cancel', async () => {
    const { w, server, platform } = await setup([approvalOf('a1', { risk: 'destructive' })])
    platform.confirm.mockResolvedValueOnce(false)
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledTimes(1)
    expect(decisions(server)).toHaveLength(0)
    expect(w.find('.agent-inline-decided').exists()).toBe(false)
  })

  it('sends one request for a double click', async () => {
    let release!: (r: Response) => void
    const a = approvalOf('a1')
    const { w, server } = await setup([a], (c) =>
      c.path === '/approvals/a1/decision' ? new Promise<Response>((r) => (release = r)) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await button(w, 'Approve').trigger('click')
    release(json({ ...a, state: 'approved' }))
    await flushPromises()
    expect(decisions(server)).toHaveLength(1)
  })

  it('offers bulk decisions for several items and sends the displayed items with their hashes', async () => {
    const items = [approvalOf('a1', { args_hash: 'h1' }), approvalOf('a2', { args_hash: 'h2' })]
    const { w, server } = await setup(items, (c) =>
      c.path === '/approvals/bulk-decision' ? json({ approvals: items }) : undefined
    )
    await button(w, 'Approve all 2').trigger('click')
    await flushPromises()
    expect(server.calls.find((c) => c.path === '/approvals/bulk-decision')!.body).toEqual({
      decision: 'approve',
      items: [
        { id: 'a1', args_hash: 'h1' },
        { id: 'a2', args_hash: 'h2' }
      ]
    })
    expect(w.findAll('.agent-inline-decided')).toHaveLength(2)
    expect(w.find('.agent-group-bulk').exists()).toBe(false)
    expect(w.emitted('settled')).toHaveLength(1)
  })

  it('has no bulk buttons for a single item', async () => {
    const { w } = await setup([approvalOf('a1')])
    expect(w.find('.agent-group-bulk').exists()).toBe(false)
  })

  it('gives ended and decided items no buttons', async () => {
    const { w } = await setup([approvalOf('a1', { job_ended: true }), approvalOf('a2', { state: 'approved' })])
    expect(w.findAll('button')).toHaveLength(0)
  })

  it('shows the error, the more line and the inbox link with the job', async () => {
    const { w } = await setup([], undefined, { error: 'Could not load', more: true })
    expect(w.find('p[role=alert]').text()).toBe('Could not load')
    expect(w.text()).toContain('More actions are waiting; see them all in the inbox.')
    const link = w.find('a.agent-inline-inbox')
    expect(link.text()).toBe('Open in approvals')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-approvals', query: { job: 'j1' } })
  })

  it('drops the mark when the reread list no longer holds the item', async () => {
    const a = approvalOf('a1')
    const { w } = await setup([a], (c) => (c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined))
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    await w.setProps({ items: [] })
    await w.setProps({ items: [a] })
    expect(w.find('.agent-inline-decided').exists()).toBe(false)
    expect(w.find('.agent-card').exists()).toBe(true)
  })
})
