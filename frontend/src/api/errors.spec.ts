import { describe, expect, it } from 'vitest'
import { ApiError, describeError, isAbortError, isUnavailable, KNOWN_CODES } from './errors'

const gettext = (s: string) => s

describe('ApiError', () => {
  it('carries status, code and retryAfter and names only the code', () => {
    const e = new ApiError(429, 'rate_limited', 7)
    expect(e).toBeInstanceOf(Error)
    expect(e.name).toBe('ApiError')
    expect(e.status).toBe(429)
    expect(e.code).toBe('rate_limited')
    expect(e.retryAfter).toBe(7)
    expect(e.message).toContain('rate_limited')
  })

  it('leaves retryAfter undefined when not given', () => {
    expect(new ApiError(500, 'internal').retryAfter).toBeUndefined()
  })
})

describe('describeError', () => {
  it('has a fixed, distinct message for every known code', () => {
    const seen = new Set<string>()
    for (const code of KNOWN_CODES) {
      const m = describeError(new ApiError(400, code), gettext)
      expect(m, code).toBeTruthy()
      seen.add(m)
    }
    expect(seen.size).toBe(KNOWN_CODES.length)
  })

  it('covers the codes the API returns', () => {
    for (const code of [
      'unauthenticated',
      'origin_rejected',
      'rate_limited',
      'invalid_request',
      'not_found',
      'conflict',
      'expired',
      'job_ended',
      'stale_arguments',
      'already_decided',
      'not_acknowledgeable',
      'limit_reached',
      'budget_exhausted',
      'too_many_pending',
      'not_enrolled',
      'credentials_invalid',
      'proactive_disabled',
      'kill_switch',
      'paused',
      'live_job',
      'daily_cap',
      'budget',
      'upstream_unavailable',
      'upstream_rejected',
      'unavailable',
      'precondition_required',
      'precondition_failed',
      'already_exists',
      'file_too_large',
      'too_large',
      'unsupported_media_type',
      'internal'
    ]) {
      expect(KNOWN_CODES).toContain(code)
    }
  })

  it('falls back to a generic message for unknown codes', () => {
    const generic = describeError(new ApiError(418, 'teapot_<b>x</b>'), gettext)
    expect(generic).toBe(describeError(new ApiError(500, 'something_else'), gettext))
    expect(generic).not.toContain('teapot')
    expect(KNOWN_CODES.map((c) => describeError(new ApiError(400, c), gettext))).not.toContain(generic)
  })

  it('does not treat prototype names as codes', () => {
    const generic = describeError(new ApiError(500, 'something_else'), gettext)
    expect(describeError(new ApiError(500, 'constructor'), gettext)).toBe(generic)
    expect(describeError(new ApiError(500, 'toString'), gettext)).toBe(generic)
  })

  it('never shows an exception message', () => {
    const generic = describeError(new ApiError(500, 'something_else'), gettext)
    expect(describeError(new Error('secret path /etc/passwd'), gettext)).toBe(generic)
    expect(describeError(new TypeError('Failed to fetch https://x/y?token=abc'), gettext)).toBe(generic)
    expect(describeError('raw string', gettext)).toBe(generic)
    expect(describeError(undefined, gettext)).toBe(generic)
    expect(describeError({ code: 'not_found', message: 'x' }, gettext)).toBe(generic)
  })

  it('translates through the given gettext', () => {
    const m = describeError(new ApiError(404, 'not_found'), (s) => `T(${s})`)
    expect(m.startsWith('T(')).toBe(true)
  })
})

describe('isAbortError', () => {
  it('recognises abort errors only', () => {
    expect(isAbortError(new DOMException('x', 'AbortError'))).toBe(true)
    const c = new AbortController()
    c.abort()
    expect(isAbortError(c.signal.reason)).toBe(true)
    expect(isAbortError(new ApiError(0, 'unavailable'))).toBe(false)
    expect(isAbortError(new Error('AbortError'))).toBe(false)
    expect(isAbortError(undefined)).toBe(false)
  })
})

describe('isUnavailable', () => {
  it.each([
    [new ApiError(0, 'unavailable', undefined, true), true],
    [new ApiError(502, 'unavailable', undefined, true), true],
    [new ApiError(0, 'unavailable'), false],
    [new ApiError(0, 'invalid_request'), false],
    [new ApiError(502, 'internal'), false],
    [new ApiError(502, 'upstream_unavailable'), false],
    [new ApiError(503, 'unavailable'), false],
    [new ApiError(504, 'internal'), false],
    [new Error('unavailable'), false],
    [undefined, false]
  ])('%#', (e, want) => {
    expect(isUnavailable(e)).toBe(want)
  })
})
