// A minimal text/event-stream parser (the WHATWG "server-sent events"
// framing) for fetch response bodies. EventSource cannot send an
// Authorization header, so the event stream is read with fetch instead.

export interface SSEEvent {
  event: string
  data: string
}

// MAX_LINE caps one line (in UTF-16 code units); a longer line, terminated
// or not, aborts the stream with an SSEError.
export const MAX_LINE = 64 * 1024
// MAX_EVENT caps the data of one event (all its data lines together).
export const MAX_EVENT = 256 * 1024

export class SSEError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'SSEError'
  }
}

export interface ParseOptions {
  // signal ends the stream: the reader is cancelled and the generator returns
  signal?: AbortSignal
  maxLine?: number
  maxEvent?: number
  // onChunk is called for every chunk read from the stream, before it is
  // parsed (a liveness signal: comments such as pings count too)
  onChunk?: () => void
}

// parseSSE yields the events of stream as they are completed by a blank
// line. Comments, `id:`, `retry:` and unknown fields are ignored; an event
// without data is not dispatched; a final event that is not followed by a
// blank line is dropped. The stream is cancelled when the consumer stops,
// the signal aborts or the parser fails.
export async function* parseSSE(
  stream: ReadableStream<Uint8Array>,
  opts: ParseOptions = {}
): AsyncGenerator<SSEEvent> {
  const maxLine = opts.maxLine ?? MAX_LINE
  const maxEvent = opts.maxEvent ?? MAX_EVENT
  const signal = opts.signal
  const reader = stream.getReader()
  const onAbort = () => {
    reader.cancel().catch(() => {})
  }
  if (signal?.aborted) {
    onAbort()
    return
  }
  signal?.addEventListener('abort', onAbort, { once: true })

  const decoder = new TextDecoder('utf-8')
  let buf = ''
  let skipLF = false // the previous chunk ended with CR, a following LF belongs to it
  let type = ''
  let data: string[] = []
  let size = 0

  // line handles one complete line; it returns an event to dispatch, if any
  const line = (l: string): SSEEvent | undefined => {
    if (l === '') {
      const ev = data.length > 0 ? { event: type || 'message', data: data.join('\n') } : undefined
      type = ''
      data = []
      size = 0
      return ev
    }
    if (l.startsWith(':')) return undefined
    const colon = l.indexOf(':')
    const field = colon < 0 ? l : l.slice(0, colon)
    let value = colon < 0 ? '' : l.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    if (field === 'event') type = value
    else if (field === 'data') {
      size += value.length + 1
      if (size > maxEvent) throw new SSEError('event stream event too large')
      data.push(value)
    }
    return undefined
  }

  try {
    for (;;) {
      const { value, done } = await reader.read()
      if (done || signal?.aborted) return
      opts.onChunk?.()
      buf += decoder.decode(value, { stream: true })
      // a chunk that decodes to nothing (empty, or the first bytes of a
      // multi-byte character) must not drop a pending CR
      if (buf.length > 0) {
        if (skipLF && buf.startsWith('\n')) buf = buf.slice(1)
        skipLF = false
      }

      let start = 0
      for (let i = 0; i < buf.length; i++) {
        const ch = buf.charCodeAt(i)
        if (ch !== 0x0a && ch !== 0x0d) continue
        if (i - start > maxLine) throw new SSEError('event stream line too long')
        const ev = line(buf.slice(start, i))
        if (ch === 0x0d) {
          if (i + 1 < buf.length) {
            if (buf.charCodeAt(i + 1) === 0x0a) i++
          } else {
            skipLF = true
          }
        }
        start = i + 1
        if (ev) {
          yield ev
          if (signal?.aborted) return
        }
      }
      buf = buf.slice(start)
      if (buf.length > maxLine) throw new SSEError('event stream line too long')
    }
  } finally {
    signal?.removeEventListener('abort', onAbort)
    reader.cancel().catch(() => {})
  }
}
