import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TextBlock from './TextBlock.vue'

// Every child of the root must be a text node or a control-character span.
const assertOnlyTextAndCtlSpans = (root: Element) => {
  for (const node of Array.from(root.childNodes)) {
    if (node.nodeType === Node.TEXT_NODE) continue
    expect(node.nodeType).toBe(Node.ELEMENT_NODE)
    const el = node as Element
    expect(el.tagName).toBe('SPAN')
    expect(el.className).toBe('agent-ctl')
    expect(el.children.length).toBe(0)
  }
}

describe('TextBlock', () => {
  it('renders hostile markup as literal text', () => {
    const s = '<script>alert(1)</script><img src=x onerror=alert(1)></data>&amp;&lt;b&gt;'
    const w = mount(TextBlock, { props: { text: s } })
    expect(w.element.textContent).toBe(s)
    expect(w.element.querySelector('script, img, b')).toBeNull()
    assertOnlyTextAndCtlSpans(w.element)
  })

  it('makes a bidi override visible in a titled span', () => {
    const w = mount(TextBlock, { props: { text: 'invoice\u202Efdp.exe' } })
    const spans = w.findAll('span.agent-ctl')
    expect(spans).toHaveLength(1)
    expect(spans[0].text()).toBe('\\u{202E}')
    expect(spans[0].attributes('title')).toContain('U+202E')
    expect(w.element.textContent).toBe('invoice\\u{202E}fdp.exe')
    expect(w.element.textContent).not.toContain('\u202E')
    assertOnlyTextAndCtlSpans(w.element)
  })

  it('makes NUL and C1 controls visible and keeps tabs and newlines', () => {
    const w = mount(TextBlock, { props: { text: 'a\u0000b\u009Bc\td\ne' } })
    expect(w.findAll('span.agent-ctl').map((s) => s.text())).toEqual(['\\u{0000}', '\\u{009B}'])
    expect(w.element.textContent).toBe('a\\u{0000}b\\u{009B}c\td\ne')
  })

  it('keeps emoji and makes the zero-width space and line separator visible', () => {
    const w = mount(TextBlock, { props: { text: 'hi \u{1F600}\u200B!\u2028' } })
    expect(w.findAll('span.agent-ctl').map((s) => s.text())).toEqual(['\\u{200B}', '\\u{2028}'])
    expect(w.element.textContent).toBe('hi \u{1F600}\\u{200B}!\\u{2028}')
  })

  it('renders very long strings in full', () => {
    const s = 'z'.repeat(100_000)
    const w = mount(TextBlock, { props: { text: s } })
    expect(w.element.textContent).toBe(s)
  })

  it('uses a pre element when mono and a span otherwise', () => {
    expect(mount(TextBlock, { props: { text: 'x', mono: true } }).element.tagName).toBe('PRE')
    expect(mount(TextBlock, { props: { text: 'x' } }).element.tagName).toBe('SPAN')
  })

  it('caps the height to maxLines and keeps the rest scrollable', () => {
    const w = mount(TextBlock, { props: { text: 'a\nb\nc', maxLines: 2 } })
    const style = (w.element as HTMLElement).style
    expect(style.maxHeight).toContain('2')
    expect(style.overflowY).toBe('auto')
    expect(w.element.textContent).toBe('a\nb\nc')
  })

  it.each([
    [undefined, 'pre-wrap'],
    ['pre', 'pre-wrap'],
    ['normal', 'normal'],
    ['nowrap', 'nowrap'],
    ['bogus', 'pre-wrap']
  ])('wrap %s gives white-space %s', (wrap, expected) => {
    const w = mount(TextBlock, { props: { text: 'x', wrap: wrap as never } })
    const style = w.attributes('style') ?? ''
    expect(style).toContain(`white-space: ${expected}`)
    if (wrap === 'nowrap') expect(style).not.toContain('overflow-wrap')
    else expect(style).toContain('overflow-wrap: anywhere')
  })

  it('renders mono with nowrap as a pre without wrapping', () => {
    const w = mount(TextBlock, { props: { text: 'x', mono: true, wrap: 'nowrap' } })
    expect(w.element.tagName).toBe('PRE')
    expect(w.attributes('style')).toContain('white-space: nowrap')
  })
})
