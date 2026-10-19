import { describe, expect, it } from 'vitest'
import { MAX_LINE, SSEError, parseSSE, type SSEEvent } from './sse'

const enc = new TextEncoder()

const streamOf = (...chunks: (string | Uint8Array)[]) =>
  new ReadableStream<Uint8Array>({
    start(c) {
      for (const ch of chunks) c.enqueue(typeof ch === 'string' ? enc.encode(ch) : ch)
      c.close()
    }
  })

const collect = async (s: ReadableStream<Uint8Array>, signal?: AbortSignal): Promise<SSEEvent[]> => {
  const out: SSEEvent[] = []
  for await (const ev of parseSSE(s, { signal })) out.push(ev)
  return out
}

describe('parseSSE', () => {
  it('parses named events and defaults the type to message', async () => {
    const evs = await collect(streamOf('event: ready\ndata: {"v":1}\n\ndata: plain\n\n'))
    expect(evs).toEqual([
      { event: 'ready', data: '{"v":1}' },
      { event: 'message', data: 'plain' }
    ])
  })

  it('joins multi-line data with newlines', async () => {
    const evs = await collect(streamOf('event: x\ndata: a\ndata:b\ndata\ndata:  c\n\n'))
    expect(evs).toEqual([{ event: 'x', data: 'a\nb\n\n c' }])
  })

  it('accepts CRLF, CR and LF line endings, also split across chunks', async () => {
    const evs = await collect(
      streamOf('event: a\r\ndata: 1\r\n\r', '\nevent: b\rdata: 2\r\r', 'event: c\ndata: 3\r', '\n\r\n')
    )
    expect(evs).toEqual([
      { event: 'a', data: '1' },
      { event: 'b', data: '2' },
      { event: 'c', data: '3' }
    ])
  })

  it('decodes multi-byte UTF-8 characters split across chunks', async () => {
    const bytes = enc.encode('data: Grüße \u{1F600}\n\n')
    // split inside the two-byte ü and inside the four-byte emoji
    const iU = bytes.indexOf(0xc3) + 1
    const iE = bytes.indexOf(0xf0) + 2
    const evs = await collect(streamOf(bytes.slice(0, iU), bytes.slice(iU, iE), bytes.slice(iE)))
    expect(evs).toEqual([{ event: 'message', data: 'Grüße \u{1F600}' }])
  })

  it('ignores comments, retry and id fields and unknown fields', async () => {
    const evs = await collect(
      streamOf(': ping\n\n:\nretry: 10\nid: 7\nfoo: bar\nevent: changed\ndata: {"kinds":["jobs"]}\n\n')
    )
    expect(evs).toEqual([{ event: 'changed', data: '{"kinds":["jobs"]}' }])
  })

  it('does not dispatch empty events and resets the type after a blank line', async () => {
    const evs = await collect(streamOf('\n\n\nevent: lonely\n\ndata: x\n\n'))
    expect(evs).toEqual([{ event: 'message', data: 'x' }])
  })

  it('drops a final event without a trailing blank line', async () => {
    const evs = await collect(streamOf('event: a\ndata: 1\n\nevent: b\ndata: 2\n'))
    expect(evs).toEqual([{ event: 'a', data: '1' }])
  })

  it('strips a leading byte order mark', async () => {
    const evs = await collect(streamOf('\uFEFFdata: x\n\n'))
    expect(evs).toEqual([{ event: 'message', data: 'x' }])
  })

  it('aborts on a line longer than the limit, terminated or not', async () => {
    const giant = 'data: ' + 'x'.repeat(MAX_LINE)
    await expect(collect(streamOf(giant + '\n\n'))).rejects.toBeInstanceOf(SSEError)
    // unterminated, delivered in small chunks
    const chunks = Array.from({ length: 70 }, () => 'y'.repeat(1024))
    await expect(collect(streamOf('data: ', ...chunks))).rejects.toBeInstanceOf(SSEError)
  })

  it('accepts a line at the limit', async () => {
    const value = 'z'.repeat(MAX_LINE - 'data: '.length)
    const evs = await collect(streamOf('data: ' + value + '\n\n'))
    expect(evs[0].data.length).toBe(value.length)
  })

  it('yields events as chunks arrive and stops when the signal aborts', async () => {
    let ctrl!: ReadableStreamDefaultController<Uint8Array>
    let cancelled = false
    const s = new ReadableStream<Uint8Array>({
      start(c) {
        ctrl = c
      },
      cancel() {
        cancelled = true
      }
    })
    const ac = new AbortController()
    const it = parseSSE(s, { signal: ac.signal })[Symbol.asyncIterator]()
    ctrl.enqueue(enc.encode('event: ready\ndata: {}\n\n'))
    expect((await it.next()).value).toEqual({ event: 'ready', data: '{}' })
    const pending = it.next()
    ac.abort()
    expect((await pending).done).toBe(true)
    expect(cancelled).toBe(true)
  })

  it('cancels the stream when the consumer stops early', async () => {
    let cancelled = false
    const s = new ReadableStream<Uint8Array>({
      start(c) {
        c.enqueue(enc.encode('data: 1\n\ndata: 2\n\n'))
      },
      cancel() {
        cancelled = true
      }
    })
    for await (const ev of parseSSE(s)) {
      expect(ev.data).toBe('1')
      break
    }
    expect(cancelled).toBe(true)
  })

  it('returns at once for an already aborted signal', async () => {
    const ac = new AbortController()
    ac.abort()
    expect(await collect(streamOf('data: 1\n\n'), ac.signal)).toEqual([])
  })
})
