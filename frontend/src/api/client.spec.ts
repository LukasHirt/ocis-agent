import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makePlatform } from '../test/platform'
import { createClient, REQUEST_TIMEOUT_MS, type ApiClient } from './client'
import { ApiError } from './errors'

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })

const errorBody = (code: string, message = 'server text') => ({ error: { code, message, request_id: 'r1' } })

type Call = [string | URL | Request, RequestInit | undefined]

const ID = '01HZX3J8K9M2N4P6Q8R0S2T4V6'

const setup = (responses: Array<Response | (() => Response | Promise<Response>)> = []) => {
  const platform = makePlatform()
  let i = 0
  platform.fetch.mockImplementation(async () => {
    const r = responses[i++] ?? json(200, {})
    return typeof r === 'function' ? r() : r
  })
  const client = createClient(platform)
  const calls = () => platform.fetch.mock.calls as unknown as Call[]
  const headersOf = (n = 0) => (calls()[n][1]?.headers ?? {}) as Record<string, string>
  return { platform, client, calls, headersOf }
}

const caught = async (p: Promise<unknown>): Promise<unknown> => {
  try {
    await p
  } catch (e) {
    return e
  }
  throw new Error('expected a rejection')
}

describe('createClient: request shape per method', () => {
  type Row = [string, (c: ApiClient) => Promise<unknown>, string, string, unknown, Record<string, string>?]
  const rows: Row[] = [
    ['getStatus', (c) => c.getStatus(), 'GET', '/status', undefined],
    ['enrol', (c) => c.enrol(), 'POST', '/enroll', {}],
    ['unenrol keep', (c) => c.unenrol(false), 'POST', '/unenroll', { delete_memory: false }],
    ['unenrol delete', (c) => c.unenrol(true), 'POST', '/unenroll', { delete_memory: true }],
    ['pause', (c) => c.pause(), 'POST', '/pause', {}],
    ['resume', (c) => c.resume(), 'POST', '/resume', {}],
    ['resume anomaly', (c) => c.resume('token_anomaly'), 'POST', '/resume', { reason: 'token_anomaly' }],
    ['renewToken', (c) => c.renewToken(), 'POST', '/token/renew', {}],
    ['revokeToken', (c) => c.revokeToken(), 'DELETE', '/token', undefined],
    ['previewRevokeAll', (c) => c.previewRevokeAll(), 'GET', '/token/revoke-all/preview', undefined],
    ['revokeAll', (c) => c.revokeAll(), 'POST', '/token/revoke-all', { confirm: true }],
    ['getSettings', (c) => c.getSettings(), 'GET', '/settings', undefined],
    [
      'putSettings',
      (c) => c.putSettings({ daily_tokens: null, models: { chat: null, proactive: 'm1' } }),
      'PUT',
      '/settings',
      { daily_tokens: null, models: { chat: null, proactive: 'm1' } }
    ],
    ['listApprovals default', (c) => c.listApprovals(), 'GET', '/approvals', undefined],
    [
      'listApprovals filtered',
      (c) => c.listApprovals({ state: 'all', jobId: ID, limit: 20, cursor: ID }),
      'GET',
      `/approvals?state=all&job_id=${ID}&limit=20&cursor=${ID}`,
      undefined
    ],
    [
      'listApprovals needing verification',
      (c) => c.listApprovals({ needsVerification: true, limit: 100 }),
      'GET',
      '/approvals?needs_verification=true&limit=100',
      undefined
    ],
    ['acknowledge', (c) => c.acknowledge(ID), 'POST', `/approvals/${ID}/acknowledge`, {}],
    ['getApproval', (c) => c.getApproval(ID), 'GET', `/approvals/${ID}`, undefined],
    [
      'decide',
      (c) => c.decide(ID, 'approve', 'h1'),
      'POST',
      `/approvals/${ID}/decision`,
      { decision: 'approve', args_hash: 'h1' }
    ],
    [
      'bulkDecide',
      (c) => c.bulkDecide('reject', [{ id: ID, args_hash: 'h1' }]),
      'POST',
      '/approvals/bulk-decision',
      { decision: 'reject', items: [{ id: ID, args_hash: 'h1' }] }
    ],
    ['listJobs', (c) => c.listJobs({ state: 'running', limit: 5 }), 'GET', '/jobs?state=running&limit=5', undefined],
    ['getJob', (c) => c.getJob(ID), 'GET', `/jobs/${ID}`, undefined],
    ['cancelJob', (c) => c.cancelJob(ID), 'POST', `/jobs/${ID}/cancel`, {}],
    ['runProactive', (c) => c.runProactive(), 'POST', '/proactive/run', {}],
    ['listConversations', (c) => c.listConversations({ cursor: ID }), 'GET', `/conversations?cursor=${ID}`, undefined],
    ['createConversation', (c) => c.createConversation(), 'POST', '/conversations', {}],
    ['createConversation titled', (c) => c.createConversation('Hi'), 'POST', '/conversations', { title: 'Hi' }],
    ['renameConversation', (c) => c.renameConversation(ID, ''), 'PUT', `/conversations/${ID}`, { title: '' }],
    ['deleteConversation', (c) => c.deleteConversation(ID), 'DELETE', `/conversations/${ID}`, undefined],
    [
      'listMessages',
      (c) => c.listMessages(ID, { limit: 100 }),
      'GET',
      `/conversations/${ID}/messages?limit=100`,
      undefined
    ],
    [
      'sendMessage',
      (c) => c.sendMessage(ID, 'hello\nworld'),
      'POST',
      `/conversations/${ID}/messages`,
      { content: 'hello\nworld' }
    ],
    ['listMemory', (c) => c.listMemory(), 'GET', '/memory', undefined],
    ['getMemory', (c) => c.getMemory('profile.md'), 'GET', '/memory/profile.md', undefined],
    ['getMemory note', (c) => c.getMemory('notes/my-note.md'), 'GET', '/memory/notes/my-note.md', undefined],
    [
      'putMemory',
      (c) => c.putMemory('notes/a.md', 'text', '"etag-1"'),
      'PUT',
      '/memory/notes/a.md',
      { content: 'text' },
      { 'If-Match': '"etag-1"' }
    ],
    [
      'createMemory',
      (c) => c.createMemory('notes/a.md', ''),
      'POST',
      '/memory',
      { name: 'notes/a.md', content: '' }
    ],
    [
      'listAudit',
      (c) => c.listAudit({ cursor: '42', limit: 10, jobId: ID, tool: 'files.write', risk: 'write' }),
      'GET',
      `/audit?cursor=42&limit=10&job_id=${ID}&tool=files.write&risk=write`,
      undefined
    ],
    [
      'listAudit newest first by kind',
      (c) => c.listAudit({ order: 'desc', cursor: 'd42', kind: 'approval_decided', limit: 50 }),
      'GET',
      '/audit?order=desc&cursor=d42&limit=50&kind=approval_decided',
      undefined
    ],
    ['verifyAudit', (c) => c.verifyAudit(), 'GET', '/audit/verify', undefined]
  ]

  it.each(rows)('%s', async (_name, call, method, path, body, extraHeaders) => {
    const { client, calls, headersOf } = setup()
    await call(client)
    expect(calls()).toHaveLength(1)
    const [url, init] = calls()[0]
    expect(url).toBe('/agent-api/v1' + path)
    expect(init?.method).toBe(method)
    const h = headersOf()
    expect(h.Authorization).toBe('Bearer test-token')
    expect(h.Accept).toBe('application/json')
    if (body === undefined) {
      expect(init?.body).toBeUndefined()
      expect(h['Content-Type']).toBeUndefined()
    } else {
      expect(h['Content-Type']).toBe('application/json')
      expect(JSON.parse(init?.body as string)).toEqual(body)
    }
    for (const [k, v] of Object.entries(extraHeaders ?? {})) {
      expect(h[k]).toBe(v)
    }
    expect(init?.signal).toBeInstanceOf(AbortSignal)
  })

  it('uses the platform backend URL', async () => {
    const platform = makePlatform({ backendUrl: '/custom/api' })
    platform.fetch.mockResolvedValue(json(200, {}))
    await createClient(platform).getStatus()
    expect(platform.fetch.mock.calls[0][0]).toBe('/custom/api/status')
  })

  it('returns the parsed body', async () => {
    const { client } = setup([json(200, { enrolled: true, extra: 1 })])
    await expect(client.getStatus()).resolves.toEqual({ enrolled: true, extra: 1 })
  })

  it('opens the event stream with an event-stream Accept and returns the raw response', async () => {
    const res = new Response('event: ready\ndata: {}\n\n', { status: 200 })
    const { client, calls, headersOf } = setup([res])
    const c = new AbortController()
    const got = await client.openEvents(c.signal)
    expect(got).toBe(res)
    expect(calls()[0][0]).toBe('/agent-api/v1/events')
    expect(calls()[0][1]?.method).toBe('GET')
    expect(headersOf().Accept).toBe('text/event-stream')
    expect(headersOf().Authorization).toBe('Bearer test-token')
    // a stream lives longer than the request timeout: the caller's signal as is
    expect(calls()[0][1]?.signal).toBe(c.signal)
  })

  it('surfaces an error of the event stream with its status and Retry-After', async () => {
    const { client } = setup([json(429, errorBody('rate_limited'), { 'Retry-After': '5' })])
    const e = (await caught(client.openEvents(new AbortController().signal))) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(429)
    expect(e.retryAfter).toBe(5)
  })
})

describe('createClient: path and query encoding', () => {
  it.each([
    ['a/b', '/approvals/a%2Fb'],
    ['%', '/approvals/%25'],
    ['a?b#c', '/approvals/a%3Fb%23c'],
    ['../x', '/approvals/..%2Fx']
  ])('encodes the id %s', async (id, path) => {
    const { client, calls } = setup()
    await client.getApproval(id)
    expect(calls()[0][0]).toBe('/agent-api/v1' + path)
  })

  it.each(['..', '.', ''])('refuses the dot-segment or empty id %j without a request', async (id) => {
    const { client, platform } = setup()
    const e = (await caught(client.getJob(id))) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.code).toBe('invalid_request')
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('refuses hostile memory names', async () => {
    const { client, platform } = setup()
    for (const name of ['..', 'notes/..', 'notes/', '', 'a/b/c']) {
      const e = (await caught(client.getMemory(name))) as ApiError
      expect(e.code, name).toBe('invalid_request')
    }
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('encodes hostile query values', async () => {
    const { client, calls } = setup()
    await client.listApprovals({ jobId: 'a&b=c', cursor: '%/..' })
    expect(calls()[0][0]).toBe('/agent-api/v1/approvals?job_id=a%26b%3Dc&cursor=%25%2F..')
  })

  it('refuses non-primitive or invalid query values without a request', async () => {
    const { client, platform } = setup()
    const bad: unknown[] = [
      { limit: 0 },
      { limit: 1.5 },
      { limit: Number.NaN },
      { limit: '5' },
      { cursor: { toString: () => 'x' } },
      { jobId: ['a'] },
      { state: 'Pending ' }
    ]
    for (const p of bad) {
      const e = (await caught(client.listApprovals(p as never))) as ApiError
      expect(e.code, JSON.stringify(p)).toBe('invalid_request')
    }
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  // the server's page caps: approvals and chat 100, jobs and audit 200
  type LimitRow = [string, (c: ApiClient, limit: number) => Promise<unknown>, number]
  const limitRows: LimitRow[] = [
    ['listApprovals', (c, limit) => c.listApprovals({ limit }), 100],
    ['listConversations', (c, limit) => c.listConversations({ limit }), 100],
    ['listMessages', (c, limit) => c.listMessages(ID, { limit }), 100],
    ['listJobs', (c, limit) => c.listJobs({ limit }), 200],
    ['listAudit', (c, limit) => c.listAudit({ limit }), 200]
  ]

  it.each(limitRows)('%s accepts a limit up to the server cap %i', async (_name, call, max) => {
    const { client, calls } = setup()
    await call(client, max)
    expect(String(calls()[0][0])).toContain(`limit=${max}`)
  })

  it.each(limitRows)('%s refuses a limit above the server cap %i without a request', async (_name, call, max) => {
    const { client, platform } = setup()
    const e = (await caught(call(client, max + 1))) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.code).toBe('invalid_request')
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('refuses audit cursors and orders the server does not issue without a request', async () => {
    const { client, platform } = setup()
    const bad: unknown[] = [
      { order: 'desc', cursor: '42' }, // an ascending cursor in the newest-first order
      { cursor: 'd42' }, // a newest-first cursor in the ascending order
      { order: 'desc', cursor: 'd' },
      { order: 'desc', cursor: 'd-1' },
      { order: 'newest' },
      { kind: 'Tool_Call' },
      { kind: 'a&b' }
    ]
    for (const p of bad) {
      const e = (await caught(client.listAudit(p as never))) as ApiError
      expect(e.code, JSON.stringify(p)).toBe('invalid_request')
    }
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('sends needs_verification only as true', async () => {
    const { client, calls, platform } = setup()
    await client.listApprovals({ needsVerification: false })
    expect(calls()[0][0]).toBe('/agent-api/v1/approvals')
    const e = (await caught(client.listApprovals({ needsVerification: 'yes' } as never))) as ApiError
    expect(e.code).toBe('invalid_request')
    expect(platform.fetch).toHaveBeenCalledTimes(1)
  })

  it('omits empty optional parameters', async () => {
    const { client, calls } = setup()
    await client.listJobs({ state: undefined, cursor: '' })
    expect(calls()[0][0]).toBe('/agent-api/v1/jobs')
  })
})

describe('createClient: tokens', () => {
  it('reads the token per call', async () => {
    const { client, platform, headersOf } = setup()
    platform.getToken.mockReturnValueOnce('tok-1').mockReturnValueOnce('tok-2')
    await client.getStatus()
    await client.getStatus()
    expect(headersOf(0).Authorization).toBe('Bearer tok-1')
    expect(headersOf(1).Authorization).toBe('Bearer tok-2')
  })

  it('throws unauthenticated without calling fetch when there is no token', async () => {
    const { client, platform } = setup()
    platform.getToken.mockReturnValue(undefined)
    const e = (await caught(client.getStatus())) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(401)
    expect(e.code).toBe('unauthenticated')
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('treats an empty token as missing', async () => {
    const { client, platform } = setup()
    platform.getToken.mockReturnValue('')
    expect(((await caught(client.getStatus())) as ApiError).code).toBe('unauthenticated')
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  describe('401', () => {
    beforeEach(() => {
      vi.useFakeTimers()
    })
    afterEach(() => {
      vi.useRealTimers()
    })

    it('retries once with a refreshed token', async () => {
      const { client, platform, calls, headersOf } = setup([
        json(401, errorBody('unauthenticated')),
        json(200, { ok: 1 })
      ])
      let token = 'old'
      platform.getToken.mockImplementation(() => token)
      const p = client.getStatus()
      await vi.advanceTimersByTimeAsync(400)
      expect(calls()).toHaveLength(1)
      token = 'new'
      await vi.advanceTimersByTimeAsync(200)
      await expect(p).resolves.toEqual({ ok: 1 })
      expect(calls()).toHaveLength(2)
      expect(headersOf(0).Authorization).toBe('Bearer old')
      expect(headersOf(1).Authorization).toBe('Bearer new')
    })

    it('retries a request with a body with the same body', async () => {
      const { client, platform, calls } = setup([json(401, errorBody('unauthenticated')), json(200, {})])
      let token = 'old'
      platform.getToken.mockImplementation(() => token)
      const p = client.decide(ID, 'approve', 'h1')
      token = 'new'
      await vi.advanceTimersByTimeAsync(200)
      await p
      expect(calls()[1][1]?.body).toBe(calls()[0][1]?.body)
      expect(calls()[1][1]?.method).toBe('POST')
    })

    it('gives up after 3 s without a new token', async () => {
      const { client, calls } = setup([json(401, errorBody('unauthenticated'))])
      const p = caught(client.getStatus())
      await vi.advanceTimersByTimeAsync(3000)
      const e = (await p) as ApiError
      expect(e).toBeInstanceOf(ApiError)
      expect(e.status).toBe(401)
      expect(e.code).toBe('unauthenticated')
      expect(calls()).toHaveLength(1)
    })

    it('does not count a vanished token as a refresh', async () => {
      const { client, platform, calls } = setup([json(401, errorBody('unauthenticated'))])
      let token: string | undefined = 'old'
      platform.getToken.mockImplementation(() => token)
      const p = caught(client.getStatus())
      token = undefined
      await vi.advanceTimersByTimeAsync(3000)
      expect(((await p) as ApiError).code).toBe('unauthenticated')
      expect(calls()).toHaveLength(1)
    })

    it('retries only once', async () => {
      const { client, platform, calls } = setup([
        json(401, errorBody('unauthenticated')),
        json(401, errorBody('unauthenticated')),
        json(200, {})
      ])
      let n = 0
      platform.getToken.mockImplementation(() => `tok-${n++}`)
      const p = caught(client.getStatus())
      await vi.advanceTimersByTimeAsync(10_000)
      expect(((await p) as ApiError).code).toBe('unauthenticated')
      expect(calls()).toHaveLength(2)
    })

    it('never navigates or notifies', async () => {
      const { client, platform } = setup([json(401, errorBody('unauthenticated'))])
      const href = window.location.href
      const p = caught(client.getStatus())
      await vi.advanceTimersByTimeAsync(3000)
      await p
      expect(window.location.href).toBe(href)
      expect(platform.notify).not.toHaveBeenCalled()
      expect(platform.confirm).not.toHaveBeenCalled()
    })

    it('stops waiting when the caller aborts', async () => {
      const { client, calls } = setup([json(401, errorBody('unauthenticated'))])
      const c = new AbortController()
      const p = caught(client.getStatus(c.signal))
      await vi.advanceTimersByTimeAsync(200)
      c.abort()
      await vi.advanceTimersByTimeAsync(200)
      const e = await p
      expect((e as DOMException).name).toBe('AbortError')
      expect(calls()).toHaveLength(1)
    })
  })
})

describe('createClient: responses and errors', () => {
  it('returns undefined for 204', async () => {
    const { client } = setup([new Response(null, { status: 204 })])
    await expect(client.unenrol(false)).resolves.toBeUndefined()
  })

  it('maps a JSON error body to status and code', async () => {
    const { client } = setup([json(409, errorBody('stale_arguments'))])
    const e = (await caught(client.decide(ID, 'approve', 'h'))) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(409)
    expect(e.code).toBe('stale_arguments')
    expect(e.retryAfter).toBeUndefined()
  })

  it.each([
    ['0', 0],
    ['5', 5],
    ['120', 120],
    [' 7 ', 7]
  ])('reads Retry-After %j', async (h, want) => {
    const { client } = setup([json(429, errorBody('rate_limited'), { 'Retry-After': h })])
    const e = (await caught(client.getStatus())) as ApiError
    expect(e.code).toBe('rate_limited')
    expect(e.retryAfter).toBe(want)
  })

  it.each(['-1', '1.5', 'soon', 'Wed, 21 Oct 2026 07:28:00 GMT', '', '99999999999999999999'])(
    'ignores an unusable Retry-After %j',
    async (h) => {
      const { client } = setup([json(429, errorBody('rate_limited'), { 'Retry-After': h })])
      const e = (await caught(client.getStatus())) as ApiError
      expect(e.retryAfter).toBeUndefined()
    }
  )

  it('does not retry a 429', async () => {
    const { client, calls } = setup([json(429, errorBody('rate_limited'), { 'Retry-After': '1' })])
    await caught(client.getStatus())
    expect(calls()).toHaveLength(1)
  })

  it.each([
    [500, 'not json', 'internal'],
    [503, '', 'unavailable'],
    [404, '{"error":"not_found"}', 'not_found'],
    [400, '{"error":{"code":5}}', 'invalid_request'],
    [409, '{"error":{"code":"Bad Code!"}}', 'conflict'],
    [410, 'null', 'expired'],
    [413, '[]', 'too_large'],
    [403, '<html>forbidden</html>', 'origin_rejected'],
    [412, '{}', 'precondition_failed'],
    [428, '{}', 'precondition_required'],
    [502, '{}', 'upstream_unavailable'],
    [418, '{}', 'internal']
  ])('derives the code of a malformed %i error body', async (status, body, code) => {
    const { client } = setup([new Response(body, { status })])
    const e = (await caught(client.getStatus())) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(status)
    expect(e.code).toBe(code)
  })

  it('fails a 2xx with a body that is not JSON as internal', async () => {
    const { client } = setup([new Response('<html>proxy</html>', { status: 200 })])
    const e = (await caught(client.getStatus())) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(502)
    expect(e.code).toBe('internal')
  })

  it('fails an empty 200 as internal', async () => {
    const { client } = setup([new Response('', { status: 200 })])
    expect(((await caught(client.getStatus())) as ApiError).code).toBe('internal')
  })

  it('maps a network failure to unavailable', async () => {
    const { client, platform } = setup()
    platform.fetch.mockRejectedValue(new TypeError('Failed to fetch https://host/agent-api/v1/status'))
    const e = (await caught(client.getStatus())) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.status).toBe(0)
    expect(e.code).toBe('unavailable')
    expect(e.message).not.toContain('Failed to fetch')
    expect(e.message).not.toContain('/agent-api')
  })

  it('rethrows the abort of the caller', async () => {
    const { client, platform } = setup()
    platform.fetch.mockImplementation(
      (_u, init) =>
        new Promise((_res, rej) => {
          init?.signal?.addEventListener('abort', () => rej(init.signal?.reason))
        })
    )
    const c = new AbortController()
    const p = caught(client.getStatus(c.signal))
    c.abort()
    const e = await p
    expect((e as DOMException).name).toBe('AbortError')
  })

  it('applies a per-request deadline (30 s by default) as unavailable', async () => {
    expect(REQUEST_TIMEOUT_MS).toBe(30_000)
    const platform = makePlatform()
    platform.fetch.mockImplementation(
      (_u, init) =>
        new Promise((_res, rej) => {
          init?.signal?.addEventListener('abort', () => rej(init.signal?.reason))
        })
    )
    const client = createClient(platform, { timeoutMs: 20 })
    for (const signal of [undefined, new AbortController().signal]) {
      const e = (await caught(client.getStatus(signal))) as ApiError
      expect(e).toBeInstanceOf(ApiError)
      expect(e.code).toBe('unavailable')
    }
  })

  it('does not call fetch when already aborted', async () => {
    const { client, platform } = setup()
    const c = new AbortController()
    c.abort()
    const e = await caught(client.getStatus(c.signal))
    expect((e as DOMException).name).toBe('AbortError')
    expect(platform.fetch).not.toHaveBeenCalled()
  })

  it('passes a signal that follows the caller', async () => {
    const { client, calls } = setup()
    const c = new AbortController()
    await client.getStatus(c.signal)
    const s = calls()[0][1]?.signal as AbortSignal
    expect(s.aborted).toBe(false)
    c.abort()
    expect(s.aborted).toBe(true)
  })

  it('never puts the server message, status text or URL into errors', async () => {
    const { client } = setup([
      new Response(JSON.stringify(errorBody('not_found', 'SERVER-SECRET /etc/passwd')), {
        status: 404,
        statusText: 'STATUS-TEXT'
      })
    ])
    const e = (await caught(client.getApproval(ID))) as ApiError
    const all = JSON.stringify({ m: e.message, s: e.stack, e, keys: Object.entries(e) })
    expect(all).not.toContain('SERVER-SECRET')
    expect(all).not.toContain('STATUS-TEXT')
    expect(all).not.toContain(ID)
    expect(all).not.toContain('/agent-api')
  })

  it('never leaks the token into errors or the console', async () => {
    const spies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) =>
      vi.spyOn(console, m).mockImplementation(() => {})
    )
    try {
      const secret = 'SECRET-TOKEN-123'
      const { client, platform } = setup([
        json(400, errorBody('invalid_request', secret)),
        new Response('not json', { status: 200 })
      ])
      platform.getToken.mockReturnValue(secret)
      const errs: unknown[] = [await caught(client.getStatus()), await caught(client.getStatus())]
      platform.fetch.mockRejectedValue(new TypeError('network ' + secret))
      errs.push(await caught(client.getStatus()))
      for (const e of errs) {
        const err = e as ApiError
        expect(err).toBeInstanceOf(ApiError)
        expect(JSON.stringify({ m: err.message, s: err.stack, e: err, keys: Object.entries(err) })).not.toContain(
          secret
        )
        expect(String(err)).not.toContain(secret)
      }
      for (const s of spies) {
        for (const args of s.mock.calls) {
          expect(JSON.stringify(args)).not.toContain(secret)
        }
      }
    } finally {
      spies.forEach((s) => s.mockRestore())
    }
  })
})

describe('createClient: audit export', () => {
  const ndjson = (body: BodyInit | null, headers: Record<string, string> = {}) =>
    new Response(body, {
      status: 200,
      headers: {
        'Content-Type': 'application/x-ndjson',
        'Content-Disposition': 'attachment; filename="audit-0a1b2c3d.jsonl"',
        ...headers
      }
    })

  // a body delivered in the given chunks, then optionally failing
  const chunked = (chunks: string[], fail = false) =>
    new ReadableStream<Uint8Array>({
      start(c) {
        for (const s of chunks) c.enqueue(new TextEncoder().encode(s))
        if (fail) c.error(new Error('connection reset SECRET-DETAIL'))
        else c.close()
      }
    })

  it('downloads with the bearer in the Authorization header and never in the URL', async () => {
    const { client, calls, headersOf } = setup([ndjson('{"seq":1}\n{"seq":2}\n')])
    const got = await client.exportAudit()
    const [url, init] = calls()[0]
    expect(url).toBe('/agent-api/v1/audit/export')
    expect(String(url)).not.toContain('test-token')
    expect(String(url)).not.toContain('?')
    expect(init?.method).toBe('GET')
    expect(headersOf().Authorization).toBe('Bearer test-token')
    expect(headersOf().Accept).toBe('application/x-ndjson')
    expect(got.filename).toBe('audit-0a1b2c3d.jsonl')
    expect(got.bytes).toBe(20)
    expect(got.blob.type).toBe('application/x-ndjson')
    expect(await got.blob.text()).toBe('{"seq":1}\n{"seq":2}\n')
  })

  it('reports the bytes received so far', async () => {
    const { client } = setup([ndjson(chunked(['{"seq":1}\n', '{"seq":2}\n']))])
    const seen: number[] = []
    await client.exportAudit((n) => seen.push(n))
    expect(seen).toEqual([10, 20])
  })

  it.each([
    ['missing', undefined],
    ['a path', 'attachment; filename="../../etc/passwd"'],
    ['another name', 'attachment; filename="evil.html"']
  ])('falls back to a fixed file name when the header is %s', async (_n, cd) => {
    const res = new Response('', { status: 200, headers: cd ? { 'Content-Disposition': cd } : {} })
    const { client } = setup([res])
    expect((await client.exportAudit()).filename).toBe('audit.jsonl')
  })

  it('accepts an empty log', async () => {
    const { client } = setup([ndjson('')])
    const got = await client.exportAudit()
    expect(got.bytes).toBe(0)
  })

  it('treats a transfer that breaks off as unavailable, never as a short file', async () => {
    const { client } = setup([ndjson(chunked(['{"seq":1}\n'], true))])
    const e = (await caught(client.exportAudit())) as ApiError
    expect(e).toBeInstanceOf(ApiError)
    expect(e.code).toBe('unavailable')
    expect(JSON.stringify({ m: e.message, e })).not.toContain('SECRET-DETAIL')
  })

  it('accepts empty chunks after the last line', async () => {
    const { client } = setup([ndjson(chunked(['{"seq":1}\n', '', '']))])
    const got = await client.exportAudit()
    expect(got.bytes).toBe(10)
    expect(await got.blob.text()).toBe('{"seq":1}\n')
  })

  it('treats a body that does not end with a whole line as unavailable', async () => {
    const { client } = setup([ndjson('{"seq":1}\n{"se')])
    const e = (await caught(client.exportAudit())) as ApiError
    expect(e.code).toBe('unavailable')
  })

  it('passes 429 with its Retry-After on', async () => {
    const { client } = setup([json(429, errorBody('rate_limited'), { 'Retry-After': '5' })])
    const e = (await caught(client.exportAudit())) as ApiError
    expect(e.status).toBe(429)
    expect(e.retryAfter).toBe(5)
  })

  it('stops with an AbortError when the caller aborts during the transfer', async () => {
    const c = new AbortController()
    let push!: ReadableStreamDefaultController<Uint8Array>
    const body = new ReadableStream<Uint8Array>({
      start(ctl) {
        push = ctl
      }
    })
    const { client } = setup([ndjson(body)])
    const p = client.exportAudit(() => c.abort(), c.signal)
    await Promise.resolve()
    push.enqueue(new TextEncoder().encode('{"seq":1}\n'))
    const e = await caught(p)
    expect((e as DOMException).name).toBe('AbortError')
  })

  it('is not cut off by the short request deadline', async () => {
    const platform = makePlatform()
    platform.fetch.mockImplementation(async (_u, init) => {
      await new Promise((r) => setTimeout(r, 60))
      if (init?.signal?.aborted) throw init.signal.reason
      return ndjson('{"seq":1}\n')
    })
    const client = createClient(platform, { timeoutMs: 20 })
    expect((await client.exportAudit()).bytes).toBe(10)
  })
})
