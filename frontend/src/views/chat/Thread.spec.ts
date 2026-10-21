import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import Thread from './Thread.vue'
import { answer, messageOf, userMessage } from '../../test/chat'
import type { Message, MessageJob } from '../../api/types'
import type { PartialAnswer } from '../../stores/chat'

const running = (): MessageJob => ({ id: 'j1', state: 'running', error: '', partial: '', partial_state: '' })
const writing = (text: string): Record<string, PartialAnswer> => ({ m1: { text, state: 'writing' } })

const base = {
  notices: {},
  partials: {},
  jobApprovals: {},
  olderCursor: null,
  olderLoading: false,
  olderError: null,
  loaded: true
}

const mountThread = (messages: Message[], over: Record<string, unknown> = {}) =>
  mount(Thread, { props: { ...base, messages, ...over } as never, attachTo: document.body })

// fakeScroll gives the scroll container a height of 200px per rendered
// message and a 100px viewport.
const fakeScroll = (el: HTMLElement) => {
  Object.defineProperty(el, 'scrollHeight', {
    configurable: true,
    get: () => el.querySelectorAll('.agent-msg').length * 200
  })
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 100 })
}

// scrollTo moves like a reader does: the browser fires a scroll event.
const scrollTo = (el: HTMLElement, top: number) => {
  el.scrollTop = top
  el.dispatchEvent(new Event('scroll'))
}

describe('Thread', () => {
  it('is a polite live log in list order', () => {
    const w = mountThread([userMessage('m1', null, 'first'), answer('m2', 'second')])
    const log = w.find('[role=log]')
    expect(log.attributes('aria-live')).toBe('polite')
    expect(w.findAll('.agent-msg-content').map((c) => c.text())).toEqual(['first', 'second'])
    w.unmount()
  })

  it('shows an empty state', () => {
    const w = mountThread([])
    expect(w.text()).toContain('No messages yet. Ask the agent something.')
    w.unmount()
  })

  it('offers older messages while a cursor exists', async () => {
    const w = mountThread([messageOf('m1')], { olderCursor: 'm1' })
    const btn = w.findAll('button').find((b) => b.text() === 'Show older messages')!
    await btn.trigger('click')
    expect(w.emitted('load-older')).toHaveLength(1)
    await w.setProps({ olderCursor: null })
    expect(w.findAll('button').some((b) => b.text() === 'Show older messages')).toBe(false)
    w.unmount()
  })

  it('shows an error of the older page with a retry', async () => {
    const w = mountThread([messageOf('m1')], { olderCursor: 'm1', olderError: 'Failed.' })
    expect(w.find('[role=alert]').text()).toBe('Failed.')
    w.unmount()
  })

  it('keeps the reading position when older messages are added above', async () => {
    const w = mountThread([messageOf('m2'), messageOf('m3')], { olderCursor: 'm2' })
    const el = w.find('.agent-thread-scroll').element as HTMLElement
    fakeScroll(el) // 400px
    scrollTo(el, 20)
    await w.setProps({ messages: [messageOf('m0'), messageOf('m1'), messageOf('m2'), messageOf('m3')] })
    await nextTick()
    expect(el.scrollTop).toBe(420) // 20 + the 400 added above
    w.unmount()
  })

  it('follows new messages when the reader is at the bottom, not when reading above', async () => {
    const w = mountThread([messageOf('m1')])
    const el = w.find('.agent-thread-scroll').element as HTMLElement
    fakeScroll(el) // 200px
    scrollTo(el, 100) // at the bottom (200 - 100)
    await w.setProps({ messages: [messageOf('m1'), answer('m2')] })
    await nextTick()
    expect(el.scrollTop).toBe(400)
    scrollTo(el, 50) // reading further up
    await w.setProps({ messages: [messageOf('m1'), answer('m2'), answer('m3')] })
    await nextTick()
    expect(el.scrollTop).toBe(50)
    w.unmount()
  })

  it('shows the newest messages when mounted with a loaded page', async () => {
    const proto = HTMLElement.prototype
    const saved = Object.getOwnPropertyDescriptor(proto, 'scrollHeight')
    Object.defineProperty(proto, 'scrollHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return this.querySelectorAll('.agent-msg').length * 200
      }
    })
    try {
      const w = mountThread([messageOf('m1'), messageOf('m2'), messageOf('m3')])
      await nextTick()
      expect((w.find('.agent-thread-scroll').element as HTMLElement).scrollTop).toBe(600)
      w.unmount()
    } finally {
      if (saved) Object.defineProperty(proto, 'scrollHeight', saved)
      else delete (proto as unknown as Record<string, unknown>).scrollHeight
    }
  })

  it('shows the partial answer under its question, not announced, without a badge', async () => {
    const w = mountThread([userMessage('m1', running()), userMessage('m2', null, 'later')], {
      partials: writing('Hel')
    })
    const bubbles = w.findAll('.agent-msg')
    expect(bubbles).toHaveLength(3)
    expect(bubbles[1].classes()).toContain('agent-msg-partial')
    const partial = w.find('.agent-msg-partial')
    expect(partial.attributes('aria-live')).toBe('off')
    expect(partial.find('.agent-md').text()).toBe('Hel')
    expect(partial.find('.agent-md-caret').exists()).toBe(true)
    expect(partial.find('.agent-msg-partial-state').exists()).toBe(false)
    expect(partial.find('.agent-chip').exists()).toBe(false)
    expect(w.find('.agent-msg-reply--working').exists()).toBe(false)
    await w.setProps({ partials: { m1: { text: '', state: 'thinking' } } })
    const busy = w.find('.agent-msg-partial .agent-msg-busy')
    expect(busy.text()).toContain('Thinking')
    expect(busy.find('.agent-dots').exists()).toBe(true)
    expect(w.find('.agent-msg-partial .agent-md').exists()).toBe(false)
    w.unmount()
  })

  it('renders a hostile partial answer as literal text with visible control characters', () => {
    const hostile = '<img src=x onerror=alert(1)><b>bold</b>\u202eevil\u200b{{ 1 + 1 }}'
    const w = mountThread([userMessage('m1', running())], { partials: writing(hostile) })
    const content = w.find('.agent-msg-partial .agent-msg-content')
    expect(content.find('img').exists()).toBe(false)
    expect(content.find('b').exists()).toBe(false)
    expect(content.find('.agent-md').text()).toBe('<img src=x onerror=alert(1)><b>bold</b>\\u{202E}evil\\u{200B}{{ 1 + 1 }}')
    expect(content.findAll('.agent-ctl')).toHaveLength(2)
    w.unmount()
  })

  it('shows no badge for an answered question', () => {
    const answered = userMessage('m1', { ...running(), state: 'succeeded' })
    const w = mountThread([answered, { ...answer('m2', 'Done'), job_id: 'j1' }])
    expect(w.text()).not.toContain('Answered')
    expect(w.find('.agent-chip').exists()).toBe(false)
    expect(w.find('.agent-msg-reply').exists()).toBe(false)
    w.unmount()
  })

  it('shows a running question without a partial as a working reply', () => {
    const w = mountThread([userMessage('m1', running())])
    expect(w.find('.agent-msg-reply--working').exists()).toBe(true)
    w.unmount()
  })

  it('shows a failed question once, as the runner message', () => {
    const failed = userMessage('m1', { ...running(), state: 'failed', error: 'Reason text' })
    const sys = messageOf('s1', { role: 'system', content: 'Reason text', job_id: 'j1' })
    const w = mountThread([failed, sys])
    expect(w.findAll('.agent-msg-reply--error')).toHaveLength(1)
    expect(w.text().split('Reason text')).toHaveLength(2)
    w.unmount()
  })

  it('shows a failed question without a runner message from its job error', () => {
    const failed = userMessage('m1', { ...running(), state: 'failed', error: 'Reason text' })
    const w = mountThread([failed])
    expect(w.findAll('.agent-msg-reply--error')).toHaveLength(1)
    expect(w.text()).toContain('Reason text')
    w.unmount()
  })

  it('announces only errors that appear after the conversation was shown', async () => {
    const old = userMessage('m1', { ...running(), id: 'j0', state: 'failed', error: 'Old' })
    const w = mountThread([old])
    expect(w.find('.agent-msg-reply--error').attributes('role')).toBeUndefined()
    const fresh = userMessage('m2', { ...running(), id: 'j2', state: 'failed', error: 'New' })
    await w.setProps({ messages: [old, fresh] })
    const errors = w.findAll('.agent-msg-reply--error')
    expect(errors).toHaveLength(2)
    expect(errors[0].attributes('role')).toBeUndefined()
    expect(errors[1].attributes('role')).toBe('alert')
    w.unmount()
  })

  it('alerts for a question that was listed while running and fails afterwards, once', async () => {
    const q = userMessage('m1', running())
    const w = mountThread([q])
    expect(w.find('.agent-msg-reply--working').exists()).toBe(true)
    const failed = userMessage('m1', { ...running(), state: 'failed', error: 'Late' })
    await w.setProps({ messages: [failed] })
    const errors = w.findAll('.agent-msg-reply--error')
    expect(errors).toHaveLength(1)
    expect(errors[0].attributes('role')).toBe('alert')
    // the runner's message that replaces the reply does not alert a second time
    const sys = messageOf('s1', { role: 'system', content: 'Late', job_id: 'j1' })
    await w.setProps({ messages: [failed, sys] })
    expect(w.findAll('.agent-msg-reply--error')).toHaveLength(1)
    expect(w.findAll('[role="alert"]')).toHaveLength(0)
    w.unmount()
  })

  it('alerts for the runner message of a job that ran while the conversation was open', async () => {
    const w = mountThread([userMessage('m1', running())])
    const failed = userMessage('m1', { ...running(), state: 'failed', error: 'Late' })
    const sys = messageOf('s1', { role: 'system', content: 'Late', job_id: 'j1' })
    await w.setProps({ messages: [failed, sys] })
    const errors = w.findAll('.agent-msg-reply--error')
    expect(errors).toHaveLength(1)
    expect(errors[0].attributes('role')).toBe('alert')
    w.unmount()
  })

  it('replaces the partial answer by the answer once, in one update', async () => {
    const w = mountThread([userMessage('m1', running())], { partials: writing('Hello') })
    const answered = { ...userMessage('m1', { ...running(), state: 'succeeded' }) }
    await w.setProps({ messages: [answered, { ...answer('m2', 'Hello world'), job_id: 'j1' }], partials: {} })
    expect(w.find('.agent-msg-partial').exists()).toBe(false)
    expect(w.findAll('.agent-msg-assistant').map((b) => b.find('.agent-msg-content').text())).toEqual(['Hello world'])
    w.unmount()
  })

  it('follows a growing partial answer only while the reader is at the bottom', async () => {
    const w = mountThread([userMessage('m1', running())], { partials: writing('a') })
    const el = w.find('.agent-thread-scroll').element as HTMLElement
    // 200px per bubble plus one per character of the partial answer
    Object.defineProperty(el, 'scrollHeight', {
      configurable: true,
      get: () =>
        el.querySelectorAll('.agent-msg').length * 200 +
        (el.querySelector('.agent-msg-partial .agent-md')?.textContent?.length ?? 0)
    })
    Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => 100 })
    scrollTo(el, 301) // at the bottom (401 - 100)
    await w.setProps({ messages: [userMessage('m1', running())], partials: writing('a'.repeat(50)) })
    await nextTick()
    expect(el.scrollTop).toBe(450)
    scrollTo(el, 10) // reading further up
    await w.setProps({ messages: [userMessage('m1', running())], partials: writing('a'.repeat(90)) })
    await nextTick()
    expect(el.scrollTop).toBe(10)
    w.unmount()
  })
})
