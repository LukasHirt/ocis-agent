// ApiError is a failed API call: the HTTP status (0 when no response
// arrived) and the server's error code. It never carries the server's
// message text, the URL, the token or the text of an underlying exception,
// so it is safe to log; users see describeError's fixed texts.
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly retryAfter?: number

  constructor(status: number, code: string, retryAfter?: number) {
    super(`ocis-agent api: ${code} (${status})`)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    if (retryAfter !== undefined) this.retryAfter = retryAfter
  }
}

export type Gettext = (msgid: string) => string

// The fixed user-facing text per error code. Every msgid is a literal so it
// can be extracted for translation.
const messages: Record<string, (t: Gettext) => string> = {
  unauthenticated: (t) => t('Your session has expired. Reload the page or sign in again.'),
  origin_rejected: (t) => t('The request was refused because it did not come from ownCloud.'),
  rate_limited: (t) => t('Too many requests. Wait a moment and try again.'),
  invalid_request: (t) => t('The request was not accepted. Check your input and try again.'),
  not_found: (t) => t('This item no longer exists.'),
  method_not_allowed: (t) => t('This action is not supported.'),
  conflict: (t) => t('This conflicts with the current state. Reload and try again.'),
  expired: (t) => t('This has expired.'),
  job_ended: (t) => t('The task has already ended, so this can no longer be decided.'),
  stale_arguments: (t) => t('The action changed since it was shown. Review it again.'),
  already_decided: (t) => t('This approval has already been decided.'),
  limit_reached: (t) => t('A limit has been reached. Try again later.'),
  budget_exhausted: (t) => t("Today's token budget is used up. It resets at midnight UTC."),
  too_many_pending: (t) => t('Too many messages are still waiting for an answer. Try again later.'),
  not_enrolled: (t) => t('The agent is not set up for you yet. Set it up first.'),
  credentials_invalid: (t) => t('The agent has lost access to your files. Reconnect it.'),
  no_login_name: (t) => t('Your account has no login name, which the agent needs.'),
  upstream_unavailable: (t) => t('ownCloud could not be reached. Try again later.'),
  upstream_rejected: (t) => t('ownCloud refused the request.'),
  unavailable: (t) => t('The agent service is not reachable right now. Try again later.'),
  precondition_required: (t) => t('The file version is missing. Reload the file and try again.'),
  precondition_failed: (t) => t('The file changed since you opened it. Reload it.'),
  already_exists: (t) => t('A file with this name already exists.'),
  file_too_large: (t) => t('The file is too large to open.'),
  not_text: (t) => t('The file is not plain text.'),
  too_large: (t) => t('This is too large to send.'),
  unsupported_media_type: (t) => t('The request was sent in a format the agent does not accept.'),
  internal: (t) => t('Something went wrong in the agent service.')
}

export const KNOWN_CODES: readonly string[] = Object.freeze(Object.keys(messages))

const generic = (t: Gettext) => t('Something went wrong. Try again later.')

// describeError returns the fixed user-facing text of an error: per code for
// an ApiError, a generic text for anything else (never the error's own text).
export const describeError = (e: unknown, gettext: Gettext): string => {
  if (e instanceof ApiError && Object.prototype.hasOwnProperty.call(messages, e.code)) {
    return messages[e.code](gettext)
  }
  return generic(gettext)
}

// isAbortError reports a cancellation (the caller's AbortSignal), which is
// not a failure to show.
export const isAbortError = (e: unknown): boolean =>
  typeof e === 'object' && e !== null && (e as { name?: unknown }).name === 'AbortError'
