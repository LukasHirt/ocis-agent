import { describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createGettext } from 'vue3-gettext'
import ReplyState from './ReplyState.vue'
import { apiErr, approvalOf } from '../../test/fixtures'
import { fakeServer, startStore } from '../../test/server'
import { PLATFORM_KEY } from '../../platform'
import type { Reply, ReplyKind } from './replyState'
import type { JobApprovals } from '../../stores/chat'

const replyOf = (kind: ReplyKind, over: Partial<Reply> = {}): Reply => ({
  kind,
  text: '',
  rawState: '',
  jobId: 'j1',
  createdAt: null,
  ...over
})
const mountReply = (props: Record<string, unknown>) => mount(ReplyState, { props: props as never })

const BUDGET = 'The agent stopped because the usage budget is used up.'
const BUDGET_DE = 'Der Agent hat angehalten, weil das Budget aufgebraucht ist.'
const mountGerman = (props: Record<string, unknown>) =>
  mount(ReplyState, {
    props: props as never,
    global: {
      plugins: [
        createGettext({
          availableLanguages: { de: 'Deutsch' },
          defaultLanguage: 'de',
          translations: { de: { [BUDGET]: BUDGET_DE } }
        })
      ]
    }
  })

describe('ReplyState', () => {
  it.each([
    ['working', 'Working', 'The agent is working on it.'],
    ['waiting', 'Waiting', 'The question is waiting to be picked up.']
  ])('shows a %s reply with three hidden dots and a text for screen readers', (kind, label, sr) => {
    const w = mountReply({ reply: replyOf(kind as ReplyKind) })
    const root = w.find('article')
    expect(root.classes()).toEqual(expect.arrayContaining(['agent-msg-assistant', 'agent-msg-reply', `agent-msg-reply--${kind}`]))
    expect(root.attributes('aria-live')).toBe('off')
    expect(root.attributes('aria-busy')).toBe('true')
    expect(w.find('.agent-msg-busy .agent-msg-state').text()).toBe(label)
    const dots = w.find('.agent-dots')
    expect(dots.attributes('aria-hidden')).toBe('true')
    expect(dots.element.children).toHaveLength(3)
    expect(w.find('.agent-sr-only').text()).toBe(sr)
  })

  it('shows the notice instead of the dots while a waiting question cannot run', () => {
    const w = mountReply({ reply: replyOf('waiting'), notice: 'paused' })
    expect(w.find('.agent-msg-notice').text()).toBe('The agent is paused; it will answer when resumed.')
    expect(w.find('.agent-dots').exists()).toBe(false)
  })

  it.each([
    ['killed', 'The agent is switched off by the operator; it will answer when it is switched on again.'],
    ['credentials', 'The agent has lost access to your files; reconnect it in Settings to get an answer.']
  ])('explains the %s notice', (notice, text) => {
    expect(mountReply({ reply: replyOf('waiting'), notice }).find('.agent-msg-notice').text()).toBe(text)
  })

  it('ignores the notice for other kinds', () => {
    expect(mountReply({ reply: replyOf('working'), notice: 'paused' }).find('.agent-msg-notice').exists()).toBe(false)
  })

  it('announces a single pending approval in the singular', async () => {
    const approvals: JobApprovals = { items: [approvalOf('a1', { job_id: 'j1' })], more: false, error: null }
    const { platform } = await startStore(fakeServer())
    const w = mount(ReplyState, {
      props: { reply: replyOf('approval'), approvals } as never,
      global: { provide: { [PLATFORM_KEY as symbol]: platform } }
    })
    expect(w.find('.agent-sr-only[aria-live="polite"]').text()).toBe('The agent needs your approval for 1 action.')
    w.unmount()
  })

  it('shows the pending approvals inline with decisions and the inbox link', async () => {
    const approvals: JobApprovals = {
      items: [
        approvalOf('a1', { job_id: 'j1', tool: 'files.write', risk: 'write' }),
        approvalOf('a2', { job_id: 'j1', tool: 'files.delete', risk: 'destructive' })
      ],
      more: true,
      error: null
    }
    const server = fakeServer()
    const { platform } = await startStore(server)
    const w = mount(ReplyState, {
      props: { reply: replyOf('approval'), approvals } as never,
      global: { provide: { [PLATFORM_KEY as symbol]: platform } }
    })
    expect(w.find('.agent-msg-reply-title').text()).toContain('Needs your approval')
    // the cards are not read aloud by the log; one short sentence is
    expect(w.find('article').attributes('aria-live')).toBe('off')
    const note = w.find('.agent-sr-only[aria-live="polite"]')
    expect(note.text()).toBe('The agent needs your approval for 2 actions.')
    expect(note.text()).not.toContain('files.write')
    expect(w.find('.agent-msg-reply-title [data-icon]').exists()).toBe(true)
    expect(w.find('.agent-msg-approvals .agent-inline-approvals').exists()).toBe(true)
    const cards = w.findAll('.agent-card--compact')
    expect(cards).toHaveLength(2)
    expect(cards[1].text()).toContain('Destructive')
    expect(w.text()).toContain('More actions are waiting')
    expect(w.findAll('button').some((b) => b.text() === 'Approve all 2')).toBe(true)
    const inbox = w.find('a.agent-inline-inbox')
    expect(JSON.parse(inbox.attributes('data-to')!)).toEqual({ name: 'agent-approvals', query: { job: 'j1' } })
    w.unmount()
  })

  it('re-emits settled after a decision attempt', async () => {
    const a = approvalOf('a1', { job_id: 'j1' })
    const server = fakeServer((c) => (c.path === '/approvals/a1/decision' ? apiErr(409, 'stale_arguments') : undefined))
    const { platform } = await startStore(server)
    const w = mount(ReplyState, {
      props: { reply: replyOf('approval'), approvals: { items: [a], more: false, error: null } } as never,
      global: { provide: { [PLATFORM_KEY as symbol]: platform } }
    })
    await w.findAll('button').find((b) => b.text() === 'Approve')!.trigger('click')
    await flushPromises()
    expect(w.emitted('settled')).toHaveLength(1)
    w.unmount()
  })

  it('shows a distinct error reply: icon instead of the robot, a bold heading and the reason', () => {
    const w = mountReply({ reply: replyOf('error', { text: BUDGET }) })
    expect(w.find('article').classes()).toContain('agent-msg-reply--error')
    const avatar = w.find('.agent-msg-avatar--error')
    expect(avatar.find('[data-icon]').exists()).toBe(true)
    expect(w.findAll('.agent-msg-avatar')).toHaveLength(1)
    expect(w.html()).not.toContain('robot')
    expect(w.find('strong').text()).toBe('Could not answer')
    expect(w.find('.agent-msg-error').text()).toBe(BUDGET)
  })

  it('translates a known reason and shows another one raw with visible control characters', () => {
    expect(mountGerman({ reply: replyOf('error', { text: BUDGET }) }).find('.agent-msg-error').text()).toBe(BUDGET_DE)
    const raw = mountGerman({ reply: replyOf('error', { text: 'odd‮ error' }) })
    expect(raw.find('.agent-msg-error').text()).toBe('odd\\u{202E} error')
  })

  it('omits the reason when there is none', () => {
    expect(mountReply({ reply: replyOf('error') }).find('.agent-msg-error').exists()).toBe(false)
  })

  it('announces an error only when asked to', () => {
    expect(mountReply({ reply: replyOf('error') }).find('article').attributes('role')).toBeUndefined()
    expect(mountReply({ reply: replyOf('error'), announce: true }).find('article').attributes('role')).toBe('alert')
    expect(mountReply({ reply: replyOf('working'), announce: true }).find('article').attributes('role')).toBeUndefined()
  })

  it('shows a cancelled reply as a small note, with its reason when there is one', () => {
    const w = mountReply({ reply: replyOf('cancelled') })
    expect(w.find('.agent-msg-reply-note').text()).toBe('Cancelled')
    expect(w.find('.agent-msg-reply-note [data-icon]').exists()).toBe(true)
    expect(w.find('.agent-msg-content').exists()).toBe(false)
    const why = mountGerman({ reply: replyOf('cancelled', { text: BUDGET }) })
    expect(why.find('.agent-msg-reply-note').text()).toContain(BUDGET_DE)
  })

  it('shows an unknown state raw', () => {
    const w = mountReply({ reply: replyOf('unknown', { rawState: 'weird‮' }) })
    expect(w.text()).toContain('Unknown state:')
    expect(w.find('.agent-msg-raw-state').text()).toBe('weird\\u{202E}')
    expect(w.find('[data-icon]').exists()).toBe(true)
  })

  it.each(['working', 'waiting', 'approval', 'cancelled', 'unknown'])('keeps the robot and no danger styling for %s', (kind) => {
    const w = mountReply({ reply: replyOf(kind as ReplyKind) })
    expect(w.find('.agent-msg-avatar--error').exists()).toBe(false)
    expect(w.html()).not.toMatch(/danger|error/)
    expect(w.find('.agent-msg-avatar [data-icon]').exists()).toBe(true)
  })
})
