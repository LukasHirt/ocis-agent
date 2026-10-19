import type { Platform } from '../platform'
import { ApiError } from './errors'
import type {
  Approval,
  ApprovalPage,
  Approvals,
  AuditPage,
  AuditVerify,
  Conversation,
  ConversationPage,
  Decision,
  DecisionItem,
  Job,
  JobDetail,
  JobPage,
  MemoryEdit,
  MemoryFile,
  MemoryList,
  MessagePage,
  Renewed,
  RevokeAllPreview,
  Revoked,
  Sent,
  Settings,
  SettingsUpdate,
  Status
} from './types'

// The default deadline of one request (a stream has none: its caller ends it).
export const REQUEST_TIMEOUT_MS = 30_000
// After a 401 the client waits this long for oCIS Web to refresh the token,
// checking every TOKEN_POLL_MS, and retries once with the new one.
export const TOKEN_WAIT_MS = 3_000
export const TOKEN_POLL_MS = 200
// The deadline of the audit export: the server streams it for up to five
// minutes.
export const EXPORT_TIMEOUT_MS = 6 * 60_000
// The file name of an export whose response names none the client accepts.
export const EXPORT_FALLBACK_NAME = 'audit.jsonl'

export interface ClientOptions {
  timeoutMs?: number // the per-request deadline, default REQUEST_TIMEOUT_MS
}

export interface PageParams {
  limit?: number
  cursor?: string
}

export interface ApprovalListParams extends PageParams {
  state?: string // a state, or "all"; default pending
  jobId?: string
}

export interface JobListParams extends PageParams {
  state?: string
}

export interface AuditListParams extends PageParams {
  jobId?: string
  tool?: string
  risk?: string
}

export type ResumeReason = 'user' | 'token_anomaly'

// AuditExport is the downloaded audit log: NDJSON, one record per line.
export interface AuditExport {
  blob: Blob
  filename: string
  bytes: number
}

export interface ApiClient {
  getStatus(signal?: AbortSignal): Promise<Status>
  enrol(signal?: AbortSignal): Promise<Status>
  unenrol(deleteMemory: boolean, signal?: AbortSignal): Promise<void>
  pause(signal?: AbortSignal): Promise<Status>
  resume(reason?: ResumeReason, signal?: AbortSignal): Promise<Status>
  renewToken(signal?: AbortSignal): Promise<Renewed>
  revokeToken(signal?: AbortSignal): Promise<Revoked>
  previewRevokeAll(signal?: AbortSignal): Promise<RevokeAllPreview>
  revokeAll(signal?: AbortSignal): Promise<Revoked>
  getSettings(signal?: AbortSignal): Promise<Settings>
  putSettings(update: SettingsUpdate, signal?: AbortSignal): Promise<Settings>
  listApprovals(params?: ApprovalListParams, signal?: AbortSignal): Promise<ApprovalPage>
  getApproval(id: string, signal?: AbortSignal): Promise<Approval>
  decide(id: string, decision: Decision, argsHash: string, signal?: AbortSignal): Promise<Approval>
  bulkDecide(decision: Decision, items: DecisionItem[], signal?: AbortSignal): Promise<Approvals>
  listJobs(params?: JobListParams, signal?: AbortSignal): Promise<JobPage>
  getJob(id: string, signal?: AbortSignal): Promise<JobDetail>
  cancelJob(id: string, signal?: AbortSignal): Promise<Job>
  listConversations(params?: PageParams, signal?: AbortSignal): Promise<ConversationPage>
  getConversation(id: string, signal?: AbortSignal): Promise<Conversation>
  createConversation(title?: string, signal?: AbortSignal): Promise<Conversation>
  renameConversation(id: string, title: string, signal?: AbortSignal): Promise<Conversation>
  deleteConversation(id: string, signal?: AbortSignal): Promise<void>
  listMessages(conversationId: string, params?: PageParams, signal?: AbortSignal): Promise<MessagePage>
  sendMessage(conversationId: string, content: string, signal?: AbortSignal): Promise<Sent>
  listMemory(signal?: AbortSignal): Promise<MemoryList>
  getMemory(name: string, signal?: AbortSignal): Promise<MemoryFile>
  putMemory(name: string, content: string, etag: string, signal?: AbortSignal): Promise<MemoryEdit>
  createMemory(name: string, content: string, signal?: AbortSignal): Promise<MemoryEdit>
  listAudit(params?: AuditListParams, signal?: AbortSignal): Promise<AuditPage>
  verifyAudit(signal?: AbortSignal): Promise<AuditVerify>
  // exportAudit downloads the whole audit log (with the bearer in the
  // Authorization header, never in the URL). onProgress gets the bytes
  // received so far; a transfer that breaks off is an error, never a
  // short file.
  exportAudit(onProgress?: (bytes: number) => void, signal?: AbortSignal): Promise<AuditExport>
  // openEvents opens GET /events and returns the response with its body
  // unread (the event-stream client parses it). No default timeout.
  openEvents(signal: AbortSignal): Promise<Response>
}

type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'

interface Req {
  method: Method
  path: string
  body?: unknown
  headers?: Record<string, string>
  signal?: AbortSignal
}

const invalid = () => new ApiError(0, 'invalid_request')

const MAX_VALUE_LEN = 512
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/

// segment encodes one path segment; empty and dot segments (which the URL
// parser would resolve, even percent-encoded) are refused.
const segment = (v: unknown): string => {
  if (typeof v !== 'string' || v === '' || v === '.' || v === '..' || v.length > MAX_VALUE_LEN) {
    throw invalid()
  }
  return encodeURIComponent(v)
}

// memoryPath maps a memory file name ("profile.md" or "notes/<slug>") to
// its route.
const memoryPath = (name: unknown): string => {
  if (typeof name !== 'string') throw invalid()
  const parts = name.split('/')
  if (parts.length === 1) return '/memory/' + segment(parts[0])
  if (parts.length === 2 && parts[0] === 'notes') return '/memory/notes/' + segment(parts[1])
  throw invalid()
}

type QueryKind = 'string' | 'name' | 'tool' | 'limit100' | 'limit200' | 'seq'

// a page limit within the server's cap of the route
const limitUpTo = (max: number) => (v: unknown) =>
  typeof v === 'number' && Number.isInteger(v) && v >= 1 && v <= max

const QUERY_RULES: Record<QueryKind, (v: unknown) => boolean> = {
  string: (v) => typeof v === 'string' && v.length <= MAX_VALUE_LEN,
  name: (v) => typeof v === 'string' && /^[a-z_]{1,32}$/.test(v),
  tool: (v) => typeof v === 'string' && /^[a-z0-9_.]{1,64}$/.test(v),
  // the server's page caps: approvals and chat 100, jobs and audit 200
  limit100: limitUpTo(100),
  limit200: limitUpTo(200),
  seq: (v) => typeof v === 'string' && /^\d{1,19}$/.test(v)
}

// query builds a query string from validated primitives; undefined and ''
// are left out.
const query = (entries: Array<[string, unknown, QueryKind]>): string => {
  const q = new URLSearchParams()
  for (const [key, value, kind] of entries) {
    if (value === undefined || value === '') continue
    if (!QUERY_RULES[kind](value)) throw invalid()
    q.append(key, String(value))
  }
  const s = q.toString()
  return s ? '?' + s : ''
}

const pageQuery = (
  p: PageParams | undefined,
  limit: 'limit100' | 'limit200'
): Array<[string, unknown, QueryKind]> => [
  ['limit', p?.limit, limit],
  ['cursor', p?.cursor, 'string']
]

const SETTINGS_KEYS = [
  'proactive_enabled',
  'watch',
  'sweep_interval',
  'quiet_hours',
  'daily_tokens',
  'models'
] as const

// the status-derived code of an error response without a usable body
const codeOfStatus = (status: number): string => {
  switch (status) {
    case 400:
      return 'invalid_request'
    case 401:
      return 'unauthenticated'
    case 403:
      return 'origin_rejected'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 410:
      return 'expired'
    case 412:
      return 'precondition_failed'
    case 413:
      return 'too_large'
    case 415:
      return 'unsupported_media_type'
    case 428:
      return 'precondition_required'
    case 429:
      return 'rate_limited'
    case 502:
      return 'upstream_unavailable'
    case 503:
    case 504:
      return 'unavailable'
  }
  return 'internal'
}

const parseRetryAfter = (h: string | null): number | undefined => {
  if (h === null) return undefined
  const v = h.trim()
  return /^\d{1,9}$/.test(v) ? Number(v) : undefined
}

const abortReason = (s: AbortSignal): unknown => s.reason ?? new DOMException('aborted', 'AbortError')

const throwIfAborted = (s: AbortSignal | undefined) => {
  if (s?.aborted) throw abortReason(s)
}

const sleep = (ms: number, signal?: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    const onAbort = () => {
      clearTimeout(timer)
      reject(abortReason(signal as AbortSignal))
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal?.addEventListener('abort', onAbort, { once: true })
  })

// withTimeout adds the default deadline to the caller's signal where the
// runtime supports it, else it uses the caller's signal alone.
const withTimeout = (signal: AbortSignal | undefined, ms: number): AbortSignal | undefined => {
  const A = AbortSignal as typeof AbortSignal & {
    timeout?: (ms: number) => AbortSignal
    any?: (signals: AbortSignal[]) => AbortSignal
  }
  if (typeof A.timeout !== 'function') return signal
  const timeout = A.timeout(ms)
  if (!signal) return timeout
  return typeof A.any === 'function' ? A.any([signal, timeout]) : signal
}

const discard = (res: Response) => {
  res.body?.cancel().catch(() => {})
}

// createClient returns the typed client of the sidecar's REST API. Every
// request reads the current token from the platform; the token is never
// kept, logged or put into an error. A 401 is retried once, after oCIS Web
// has refreshed the token; nothing else is retried, and the client never
// logs out or navigates.
export const createClient = (platform: Platform, opts: ClientOptions = {}): ApiClient => {
  const base = platform.backendUrl
  const timeoutMs = opts.timeoutMs ?? REQUEST_TIMEOUT_MS

  const fetchOnce = async (
    req: Req,
    body: string | undefined,
    token: string,
    accept: string,
    signal: AbortSignal | undefined
  ): Promise<Response> => {
    const headers: Record<string, string> = {
      ...req.headers,
      Authorization: 'Bearer ' + token,
      Accept: accept
    }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    const init: RequestInit = { method: req.method, headers }
    if (body !== undefined) init.body = body
    if (signal) init.signal = signal
    try {
      return await platform.fetch(base + req.path, init)
    } catch {
      throwIfAborted(req.signal)
      throw new ApiError(0, 'unavailable')
    }
  }

  // waitForNewToken polls the platform for a token other than old.
  const waitForNewToken = async (old: string, signal: AbortSignal | undefined) => {
    for (let waited = 0; waited < TOKEN_WAIT_MS; waited += TOKEN_POLL_MS) {
      await sleep(TOKEN_POLL_MS, signal)
      const t = platform.getToken()
      if (t && t !== old) return t
    }
    return undefined
  }

  const readText = async (res: Response, signal: AbortSignal | undefined): Promise<string> => {
    try {
      return await res.text()
    } catch {
      throwIfAborted(signal)
      throw new ApiError(0, 'unavailable')
    }
  }

  const errorOf = async (res: Response, signal: AbortSignal | undefined): Promise<ApiError> => {
    const retryAfter = parseRetryAfter(res.headers.get('Retry-After'))
    let code = codeOfStatus(res.status)
    const text = await readText(res, signal).catch((e) => {
      if (signal?.aborted) throw e
      return ''
    })
    try {
      const doc: unknown = JSON.parse(text)
      const c =
        typeof doc === 'object' && doc !== null
          ? (doc as { error?: { code?: unknown } }).error?.code
          : undefined
      if (typeof c === 'string' && /^[a-z][a-z0-9_]{0,63}$/.test(c)) code = c
    } catch {
      // not JSON: keep the status-derived code
    }
    return new ApiError(res.status, code, retryAfter)
  }

  // send makes the request (and the one 401 retry) and returns a 2xx
  // response; anything else is thrown as an ApiError.
  const send = async (req: Req, accept: string, timeout: number | false): Promise<Response> => {
    throwIfAborted(req.signal)
    const token = platform.getToken()
    if (!token) throw new ApiError(401, 'unauthenticated')
    const body = req.body === undefined ? undefined : JSON.stringify(req.body)
    const signalOf = () => (timeout === false ? req.signal : withTimeout(req.signal, timeout))

    let res = await fetchOnce(req, body, token, accept, signalOf())
    if (res.status === 401) {
      discard(res)
      const next = await waitForNewToken(token, req.signal)
      if (!next) throw new ApiError(401, 'unauthenticated')
      res = await fetchOnce(req, body, next, accept, signalOf())
      if (res.status === 401) {
        discard(res)
        throw new ApiError(401, 'unauthenticated')
      }
    }
    if (!res.ok) throw await errorOf(res, req.signal)
    return res
  }

  const call = async <T>(req: Req): Promise<T> => {
    const res = await send(req, 'application/json', timeoutMs)
    if (res.status === 204) {
      discard(res)
      return undefined as T
    }
    const text = await readText(res, req.signal)
    let v: unknown
    try {
      v = JSON.parse(text)
    } catch {
      throw new ApiError(502, 'internal')
    }
    if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new ApiError(502, 'internal')
    return v as T
  }

  // readExport reads the export body in chunks, reporting progress. The
  // server aborts the connection on a failure, so a body that breaks off or
  // does not end with a whole line is an error.
  const readExport = async (
    res: Response,
    onProgress: ((bytes: number) => void) | undefined,
    signal: AbortSignal | undefined
  ): Promise<{ parts: Uint8Array[]; bytes: number }> => {
    const parts: Uint8Array[] = []
    let bytes = 0
    let lastByte = -1 // of the last non-empty chunk
    try {
      if (res.body && typeof res.body.getReader === 'function') {
        const reader = res.body.getReader()
        const onAbort = () => {
          reader.cancel().catch(() => {})
        }
        signal?.addEventListener('abort', onAbort, { once: true })
        try {
          for (;;) {
            throwIfAborted(signal)
            const { done, value } = await reader.read()
            if (done) break
            if (value.byteLength > 0) lastByte = value[value.byteLength - 1]
            parts.push(value)
            bytes += value.byteLength
            onProgress?.(bytes)
          }
        } finally {
          signal?.removeEventListener('abort', onAbort)
        }
      } else {
        const buf = new Uint8Array(await res.arrayBuffer())
        parts.push(buf)
        bytes = buf.byteLength
        if (bytes > 0) lastByte = buf[bytes - 1]
        onProgress?.(bytes)
      }
    } catch {
      throwIfAborted(signal)
      throw new ApiError(0, 'unavailable')
    }
    throwIfAborted(signal)
    // a cut is mostly detected by the transport error above; a body that
    // does not end with a whole line is refused as well
    if (bytes > 0 && lastByte !== 0x0a) throw new ApiError(0, 'unavailable')
    return { parts, bytes }
  }

  const exportName = (res: Response): string => {
    const m = /^attachment; filename="(audit-[0-9a-f]{8}\.jsonl)"$/.exec(res.headers.get('Content-Disposition') ?? '')
    return m ? m[1] : EXPORT_FALLBACK_NAME
  }

  const get = <T>(path: string, signal?: AbortSignal) => call<T>({ method: 'GET', path, signal })
  const post = <T>(path: string, body: unknown, signal?: AbortSignal) =>
    call<T>({ method: 'POST', path, body, signal })

  const decision = (d: unknown): Decision => {
    if (d !== 'approve' && d !== 'reject') throw invalid()
    return d
  }
  const text = (v: unknown): string => {
    if (typeof v !== 'string') throw invalid()
    return v
  }

  return {
    getStatus: (signal) => get('/status', signal),
    enrol: (signal) => post('/enroll', {}, signal),
    unenrol: async (deleteMemory, signal) => {
      if (typeof deleteMemory !== 'boolean') throw invalid()
      return post('/unenroll', { delete_memory: deleteMemory }, signal)
    },
    pause: (signal) => post('/pause', {}, signal),
    resume: async (reason, signal) => {
      if (reason !== undefined && reason !== 'user' && reason !== 'token_anomaly') throw invalid()
      return post('/resume', reason === undefined ? {} : { reason }, signal)
    },
    renewToken: (signal) => post('/token/renew', {}, signal),
    revokeToken: (signal) => call({ method: 'DELETE', path: '/token', signal }),
    previewRevokeAll: (signal) => get('/token/revoke-all/preview', signal),
    revokeAll: (signal) => post('/token/revoke-all', { confirm: true }, signal),
    getSettings: (signal) => get('/settings', signal),
    putSettings: async (update, signal) => {
      const body: Record<string, unknown> = {}
      for (const k of SETTINGS_KEYS) {
        if (update?.[k] !== undefined) body[k] = update[k]
      }
      return call({ method: 'PUT', path: '/settings', body, signal })
    },
    listApprovals: async (params, signal) =>
      get(
        '/approvals' +
          query([
            ['state', params?.state, 'name'],
            ['job_id', params?.jobId, 'string'],
            ...pageQuery(params, 'limit100')
          ]),
        signal
      ),
    getApproval: async (id, signal) => get('/approvals/' + segment(id), signal),
    decide: async (id, d, argsHash, signal) =>
      post(
        '/approvals/' + segment(id) + '/decision',
        { decision: decision(d), args_hash: text(argsHash) },
        signal
      ),
    bulkDecide: async (d, items, signal) => {
      if (!Array.isArray(items)) throw invalid()
      return post(
        '/approvals/bulk-decision',
        {
          decision: decision(d),
          items: items.map((it) => ({ id: text(it?.id), args_hash: text(it?.args_hash) }))
        },
        signal
      )
    },
    listJobs: async (params, signal) =>
      get('/jobs' + query([['state', params?.state, 'name'], ...pageQuery(params, 'limit200')]), signal),
    getJob: async (id, signal) => get('/jobs/' + segment(id), signal),
    cancelJob: async (id, signal) => post('/jobs/' + segment(id) + '/cancel', {}, signal),
    listConversations: async (params, signal) => get('/conversations' + query(pageQuery(params, 'limit100')), signal),
    getConversation: async (id, signal) => get('/conversations/' + segment(id), signal),
    createConversation: async (title, signal) =>
      post('/conversations', title === undefined ? {} : { title: text(title) }, signal),
    renameConversation: async (id, title, signal) =>
      call({ method: 'PUT', path: '/conversations/' + segment(id), body: { title: text(title) }, signal }),
    deleteConversation: async (id, signal) =>
      call({ method: 'DELETE', path: '/conversations/' + segment(id), signal }),
    listMessages: async (id, params, signal) =>
      get('/conversations/' + segment(id) + '/messages' + query(pageQuery(params, 'limit100')), signal),
    sendMessage: async (id, content, signal) =>
      post('/conversations/' + segment(id) + '/messages', { content: text(content) }, signal),
    listMemory: (signal) => get('/memory', signal),
    getMemory: async (name, signal) => get(memoryPath(name), signal),
    putMemory: async (name, content, etag, signal) => {
      if (typeof etag !== 'string' || etag === '' || etag.length > 256 || CONTROL.test(etag)) throw invalid()
      return call({
        method: 'PUT',
        path: memoryPath(name),
        body: { content: text(content) },
        headers: { 'If-Match': etag },
        signal
      })
    },
    createMemory: async (name, content, signal) => {
      memoryPath(name) // the same name rules as the routes
      return post('/memory', { name, content: text(content) }, signal)
    },
    listAudit: async (params, signal) =>
      get(
        '/audit' +
          query([
            ['cursor', params?.cursor, 'seq'],
            ['limit', params?.limit, 'limit200'],
            ['job_id', params?.jobId, 'string'],
            ['tool', params?.tool, 'tool'],
            ['risk', params?.risk, 'name']
          ]),
        signal
      ),
    verifyAudit: (signal) => get('/audit/verify', signal),
    exportAudit: async (onProgress, signal) => {
      const req: Req = { method: 'GET', path: '/audit/export', signal }
      const res = await send(req, 'application/x-ndjson', EXPORT_TIMEOUT_MS)
      const { parts, bytes } = await readExport(res, onProgress, signal)
      return { blob: new Blob(parts as BlobPart[], { type: 'application/x-ndjson' }), filename: exportName(res), bytes }
    },
    openEvents: (signal) => send({ method: 'GET', path: '/events', signal }, 'text/event-stream', false)
  }
}
