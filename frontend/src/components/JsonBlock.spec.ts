import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import JsonBlock from './JsonBlock.vue'

const pre = (w: ReturnType<typeof mount>) => w.find('pre')

describe('JsonBlock', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('pretty-prints a value with two-space indent', () => {
    const w = mount(JsonBlock, { props: { value: { path: '/a', n: [1, 2] } } })
    expect(pre(w).text()).toBe('{\n  "path": "/a",\n  "n": [\n    1,\n    2\n  ]\n}')
  })

  it('pretty-prints raw JSON and keeps the stored key order', () => {
    const w = mount(JsonBlock, { props: { raw: '{"z":1,"a":{"y":true,"b":null}}' } })
    expect(pre(w).text()).toBe('{\n  "z": 1,\n  "a": {\n    "y": true,\n    "b": null\n  }\n}')
  })

  it('shows invalid raw JSON as text', () => {
    const raw = '{"path": "/a", <script>x</script>'
    const w = mount(JsonBlock, { props: { raw } })
    expect(pre(w).text()).toBe(raw)
    expect(w.find('script').exists()).toBe(false)
  })

  it('keeps big numbers exactly as stored in raw JSON', () => {
    const raw = '{"size":12345678901234567890123,"f":1.0000000000000000001,"e":1e400}'
    const w = mount(JsonBlock, { props: { raw } })
    const text = pre(w).text()
    expect(text).toContain('"size": 12345678901234567890123')
    expect(text).toContain('"f": 1.0000000000000000001')
    expect(text).toContain('"e": 1e400')
  })

  it('keeps escapes and unicode in strings as stored and makes controls visible', () => {
    const raw = '{"name":"r\\u00e9sum\\u00e9.txt","evil":"a\u202Eb","q":"say \\"hi\\""}'
    const w = mount(JsonBlock, { props: { raw } })
    const text = pre(w).text()
    expect(text).toContain('"name": "r\\u00e9sum\\u00e9.txt"')
    expect(text).toContain('"q": "say \\"hi\\""')
    expect(text).toContain('"evil": "a\\u{202E}b"')
    expect(w.findAll('span.agent-ctl')).toHaveLength(1)
  })

  it('renders unicode values from an object', () => {
    const w = mount(JsonBlock, { props: { value: { t: '\u{1F600} \u00E9', bidi: '\u202E' } } })
    expect(pre(w).text()).toContain('"t": "\u{1F600} \u00E9"')
    expect(w.findAll('span.agent-ctl')).toHaveLength(1)
  })

  it('does not truncate large values', () => {
    const big = 'x'.repeat(300_000)
    const w = mount(JsonBlock, { props: { value: { big } } })
    expect(pre(w).text()).toContain(big)
  })

  it('caps the height with a scroll', () => {
    const w = mount(JsonBlock, { props: { value: {} } })
    const style = (pre(w).element as HTMLElement).style
    expect(style.maxHeight).not.toBe('')
    expect(style.overflow).toBe('auto')
  })

  it('copies the shown text with the clipboard API', async () => {
    const writeText = vi.fn(async () => {})
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    const w = mount(JsonBlock, { props: { raw: '{"a":1}' } })
    await w.find('button').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('{\n  "a": 1\n}')
  })

  it('copies the original text, not the escaped view', async () => {
    const writeText = vi.fn(async () => {})
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    const w = mount(JsonBlock, { props: { raw: 'bad\u202E' } })
    await w.find('button').trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('bad\u202E')
  })

  it('shows Copied for a while and clears its timer on unmount', async () => {
    vi.useFakeTimers()
    try {
      const writeText = vi.fn(async () => {})
      vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
      const w = mount(JsonBlock, { props: { raw: '{}' } })
      await w.find('button').trigger('click')
      await flushPromises()
      expect(w.find('button').text()).toBe('Copied')
      expect(vi.getTimerCount()).toBe(1)
      w.unmount()
      expect(vi.getTimerCount()).toBe(0)
    } finally {
      vi.useRealTimers()
    }
  })

  it('hides the copy button without a clipboard API', () => {
    vi.stubGlobal('navigator', { ...navigator, clipboard: undefined })
    const w = mount(JsonBlock, { props: { raw: '{}' } })
    expect(w.find('button').exists()).toBe(false)
  })
})
