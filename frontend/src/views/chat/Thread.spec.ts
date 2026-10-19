import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import Thread from './Thread.vue'
import { answer, messageOf, userMessage } from '../../test/chat'
import type { Message } from '../../api/types'

const base = {
  notices: {},
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
})
