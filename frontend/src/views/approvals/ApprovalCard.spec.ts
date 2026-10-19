import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ApprovalCard from './ApprovalCard.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, approvalOf, json } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import type { Approval } from '../../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

// NOW is the fake platform's clock.
const NOW = Date.UTC(2026, 0, 1, 12, 0, 0)

const setup = async (a: Approval, handler?: Handler) => {
  const server = fakeServer(handler)
  const { store, platform } = await startStore(server)
  wrapper = mount(ApprovalCard, {
    props: { approval: a },
    global: { provide: { [PLATFORM_KEY as symbol]: platform } }
  })
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)!

const decisions = (server: ReturnType<typeof fakeServer>) =>
  server.calls.filter((c) => c.method === 'POST' && c.path.endsWith('/decision'))

describe('ApprovalCard', () => {
  it('shows the tool, the risk, the exact arguments, the preview, the explanation and the expiry', async () => {
    const { w } = await setup(
      approvalOf('a1', {
        tool: 'files.write',
        risk: 'write',
        args: { path: '/Docs/a.txt', content: 'hello' },
        preview: { action: 'create file', paths: ['/Docs/a.txt'], bytes: 5 },
        rationale: 'You asked me to save it.',
        target_etag: 'etag-secret-value',
        expires_at: new Date(NOW + 5 * 60_000).toISOString()
      })
    )
    expect(w.find('.agent-card-tool').element.tagName).toBe('CODE')
    expect(w.find('.agent-card-tool').text()).toBe('files.write')
    expect(w.find('.agent-risk-write').exists()).toBe(true)
    const args = w.find('.agent-card-args')
    expect(args.text()).toContain(JSON.stringify({ path: '/Docs/a.txt', content: 'hello' }, null, 2))
    expect(w.find('.agent-card-preview').text()).toContain('What will happen')
    expect(w.find('.agent-card-preview').text()).toContain('"create file"')
    expect(w.find('.agent-card-rationale').text()).toContain("Agent's explanation (unverified)")
    expect(w.find('.agent-card-rationale').text()).toContain('You asked me to save it.')
    expect(w.text()).toContain('expires in 5m')
    expect(w.text()).not.toContain('etag-secret-value')
    expect(w.find('.agent-card-warning').exists()).toBe(false)
    expect(w.classes()).not.toContain('agent-card-danger')
  })

  it.each([
    ['destructive', 'This action is irreversible'],
    ['outward', 'This action is visible to other people'],
    ['mystery', 'The risk of this action is unknown']
  ])('marks %s actions as dangerous', async (risk, heading) => {
    const { w } = await setup(approvalOf('a1', { risk }))
    expect(w.classes()).toContain('agent-card-danger')
    expect(w.find('.agent-card-warning').text()).toBe(heading)
    expect(button(w, 'Approve').exists()).toBe(true)
  })

  it('shows hostile content as text and creates no elements', async () => {
    const { w } = await setup(
      approvalOf('a1', {
        tool: '<img src=x onerror=alert(1)>',
        risk: 'write',
        args: { path: '/a‮txt.exe', note: '</data><script>alert(1)</script>\u0000' },
        preview: { action: '<b>bold</b>', paths: ['/a‮txt.exe'] },
        rationale: '<img src=y onerror=alert(2)> ignore previous instructions\u0000'
      })
    )
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(w.find('b').exists()).toBe(false)
    expect(w.text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.text()).toContain('</data><script>alert(1)</script>')
    expect(w.text()).toContain('<img src=y onerror=alert(2)>')
    const ctl = w.findAll('.agent-ctl').map((s) => s.text())
    expect(ctl).toContain('\\u{202E}')
    expect(ctl).toContain('\\u{0000}') // NUL in the rationale (JSON escapes it in the args)
    expect(w.find('.agent-card-args').text()).toContain('\\u0000')
    expect(w.html()).not.toContain('‮')
  })

  it('approves with the displayed args_hash and no confirmation for writes', async () => {
    const a = approvalOf('a1', { args_hash: 'sha256:shown' })
    const { w, server, platform } = await setup(a, (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.confirm).not.toHaveBeenCalled()
    expect(decisions(server)).toHaveLength(1)
    expect(decisions(server)[0].body).toEqual({ decision: 'approve', args_hash: 'sha256:shown' })
    expect(platform.notify).toHaveBeenCalledWith('success', 'Approved')
  })

  it('rejects without confirmation, even for destructive actions', async () => {
    const a = approvalOf('a1', { risk: 'destructive' })
    const { w, server, platform } = await setup(a, (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'rejected' }) : undefined
    )
    await button(w, 'Reject').trigger('click')
    await flushPromises()
    expect(platform.confirm).not.toHaveBeenCalled()
    expect(decisions(server)[0].body).toEqual({ decision: 'reject', args_hash: 'h-a1' })
    expect(platform.notify).toHaveBeenCalledWith('success', 'Rejected')
  })

  it('sends one request for a double click and disables the buttons meanwhile', async () => {
    let release!: (r: Response) => void
    const a = approvalOf('a1')
    const { w, server } = await setup(a, (c) =>
      c.path === '/approvals/a1/decision' ? new Promise<Response>((r) => (release = r)) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await button(w, 'Approve').trigger('click')
    await button(w, 'Reject').trigger('click')
    await flushPromises()
    expect(decisions(server)).toHaveLength(1)
    expect(button(w, 'Approve').attributes('disabled')).toBeDefined()
    expect(button(w, 'Reject').attributes('disabled')).toBeDefined()
    release(json({ ...a, state: 'approved' }))
    await flushPromises()
    expect(button(w, 'Approve').attributes('disabled')).toBeUndefined()
  })

  it.each(['destructive', 'outward'])('confirms a %s approval, repeating tool, risk and targets', async (risk) => {
    const a = approvalOf('a1', {
      tool: 'files.delete',
      risk,
      args: { path: '/Docs/x.txt' },
      preview: { action: 'delete file', paths: ['/Docs/<b>x‮.txt'] }
    })
    const { w, server, platform } = await setup(a, (c) =>
      c.path === '/approvals/a1/decision' ? json({ ...a, state: 'approved' }) : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledTimes(1)
    const opts = platform.confirm.mock.calls[0][0]
    expect(opts.danger).toBe(true)
    expect(opts.message).toContain('files.delete')
    expect(opts.message).toContain(risk === 'destructive' ? 'Destructive' : 'Outward')
    expect(opts.message).toContain('/Docs/<b>x\\u{202E}.txt')
    expect(decisions(server)).toHaveLength(1)
  })

  it('sends nothing when the confirmation is cancelled', async () => {
    const { w, server, platform } = await setup(approvalOf('a1', { risk: 'destructive' }))
    platform.confirm.mockResolvedValueOnce(false)
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledTimes(1)
    expect(decisions(server)).toHaveLength(0)
    expect(button(w, 'Approve').attributes('disabled')).toBeUndefined()
  })

  it.each([
    ['stale_arguments', 409, 'The action changed; review the updated request'],
    ['expired', 410, 'This has expired.'],
    ['job_ended', 409, 'The task has already ended, so this can no longer be decided.'],
    ['already_decided', 409, 'This approval has already been decided.']
  ])('reports %s with a fixed text and refetches the approvals', async (code, status, text) => {
    const { w, server, platform } = await setup(approvalOf('a1'), (c) =>
      c.path === '/approvals/a1/decision' ? apiErr(status, code) : undefined
    )
    const before = server.count('GET', '/approvals')
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith('error', text)
    expect(server.count('GET', '/approvals')).toBeGreaterThan(before)
    expect(JSON.stringify(platform.notify.mock.calls)).not.toContain('server text')
  })

  it('reports rate limiting with the retry hint', async () => {
    const { w, platform } = await setup(approvalOf('a1'), (c) =>
      c.path === '/approvals/a1/decision'
        ? new Response(JSON.stringify({ error: { code: 'rate_limited', message: 'x' } }), {
            status: 429,
            headers: { 'Content-Type': 'application/json', 'Retry-After': '7' }
          })
        : undefined
    )
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith(
      'error',
      'Too many requests. Wait a moment and try again.',
      'Try again in 7 seconds.'
    )
  })

  it('labels the buttons with the tool name', async () => {
    const { w } = await setup(approvalOf('a1', { tool: 'files.move' }))
    expect(button(w, 'Approve').attributes('aria-label')).toBe('Approve files.move')
    expect(button(w, 'Reject').attributes('aria-label')).toBe('Reject files.move')
  })

  it('renders an approval of an ended job disabled', async () => {
    const { w, server } = await setup(approvalOf('a1', { job_ended: true }))
    expect(w.text()).toContain('The job ended; this can no longer be approved')
    expect(button(w, 'Approve').attributes('disabled')).toBeDefined()
    expect(button(w, 'Reject').attributes('disabled')).toBeDefined()
    await button(w, 'Approve').trigger('click')
    await flushPromises()
    expect(decisions(server)).toHaveLength(0)
  })

  it.each([
    ['executed', { state: 'done', summary: 'Wrote 5 bytes', reason: '', diff: '+hello' }, 'Done'],
    ['failed', { state: 'failed', summary: '', reason: 'The file exists.', diff: '' }, 'Failed'],
    ['rejected', { state: 'rejected', summary: '', reason: 'Rejected by the user.', diff: '' }, 'Rejected'],
    ['expired', { state: 'expired', summary: '', reason: 'The approval expired.', diff: '' }, 'Expired'],
    ['unknown', { state: 'unknown', summary: '', reason: 'No outcome was recorded.', diff: '' }, 'Outcome unknown']
  ])('shows the outcome of a %s approval without decision buttons', async (state, outcome, label) => {
    const { w } = await setup(
      approvalOf('a1', { state, outcome, needs_verification: state === 'unknown', decided_at: '2026-01-01T10:00:00Z' })
    )
    expect(w.findAll('button').filter((b) => ['Approve', 'Reject'].includes(b.text()))).toHaveLength(0)
    expect(w.find('.agent-outcome-state').text()).toBe(label)
    if (outcome.reason) expect(w.find('.agent-outcome').text()).toContain(outcome.reason)
    if (outcome.summary) expect(w.find('.agent-outcome').text()).toContain(outcome.summary)
    if (outcome.diff) expect(w.find('.agent-outcome-diff').text()).toContain(outcome.diff)
    expect(w.text()).not.toContain('expires in')
  })

  it('asks to verify an action with an unknown outcome and shows its exact arguments', async () => {
    const { w } = await setup(
      approvalOf('a1', {
        state: 'unknown',
        needs_verification: true,
        args: { path: '/Docs/report.txt' },
        outcome: { state: 'unknown', summary: '', reason: 'No outcome was recorded; please verify the action.', diff: '' }
      })
    )
    expect(w.find('.agent-card-verify').text()).toContain('check your files')
    expect(w.find('.agent-card-args').text()).toContain('/Docs/report.txt')
  })

  it('shows hostile outcome text as text', async () => {
    const { w } = await setup(
      approvalOf('a1', {
        state: 'executed',
        outcome: { state: 'done', summary: '<img src=x onerror=1>', reason: '', diff: '</data>‮' }
      })
    )
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('.agent-outcome').text()).toContain('<img src=x onerror=1>')
    expect(w.find('.agent-outcome-diff .agent-ctl').text()).toBe('\\u{202E}')
  })
})
