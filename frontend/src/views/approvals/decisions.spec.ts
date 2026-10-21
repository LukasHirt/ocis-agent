import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { useDecisions, type Decisions } from './decisions'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, approvalOf, json } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (handler?: Handler) => {
  const server = fakeServer(handler)
  const { platform } = await startStore(server)
  let d!: Decisions
  wrapper = mount(
    defineComponent({
      setup() {
        d = useDecisions()
        return () => h('div')
      }
    }),
    { global: { provide: { [PLATFORM_KEY as symbol]: platform } } }
  )
  return { d, platform }
}

describe('decisions return values', () => {
  it('is true only for an accepted decision', async () => {
    const a = approvalOf('a1')
    const { d } = await setup((c) => (c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined))
    expect(await d.decide(a, 'approve')).toBe(true)
  })

  it('is false for a cancelled confirmation, an ended job, a decided one and an error', async () => {
    const { d, platform } = await setup((c) => (c.path === '/approvals/a2/decision' ? apiErr(409, 'stale_arguments') : undefined))
    platform.confirm.mockResolvedValueOnce(false)
    expect(await d.decide(approvalOf('a1', { risk: 'destructive' }), 'approve')).toBe(false)
    expect(await d.decide(approvalOf('a1', { job_ended: true }), 'approve')).toBe(false)
    expect(await d.decide(approvalOf('a1', { state: 'approved' }), 'approve')).toBe(false)
    expect(await d.decide(approvalOf('a2'), 'approve')).toBe(false)
  })

  it('is false for a second call while the first is in flight', async () => {
    let release!: (r: Response) => void
    const a = approvalOf('a1')
    const { d } = await setup((c) => (c.path === '/approvals/a1/decision' ? new Promise<Response>((r) => (release = r)) : undefined))
    const first = d.decide(a, 'approve')
    expect(await d.decide(a, 'approve')).toBe(false)
    release(json({ ...a, state: 'approved' }))
    expect(await first).toBe(true)
  })

  it('decideAll is true on success and false on cancel, error or an empty list', async () => {
    const items = [approvalOf('a1'), approvalOf('a2', { risk: 'destructive' })]
    const { d, platform } = await setup((c) =>
      c.path === '/approvals/bulk-decision' && (c.body as { decision: string }).decision === 'reject'
        ? apiErr(409, 'stale_arguments')
        : c.path === '/approvals/bulk-decision'
          ? json({ approvals: items })
          : undefined
    )
    platform.confirm.mockResolvedValueOnce(false)
    expect(await d.decideAll(items, 'approve')).toBe(false)
    expect(await d.decideAll(items, 'approve')).toBe(true)
    expect(await d.decideAll(items, 'reject')).toBe(false)
    expect(await d.decideAll([], 'approve')).toBe(false)
    await flushPromises()
  })
})
