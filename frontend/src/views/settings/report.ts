import { useGettext } from 'vue3-gettext'
import { usePlatform } from '../../platform'
import { ApiError, describeError } from '../../api/errors'

export interface ErrorText {
  message: string // the fixed text of the error code
  detail?: string // when to try again after rate_limited
}

// useReport turns failures of the settings actions into fixed texts: the
// code's message and, for rate_limited with Retry-After, when to retry.
// Server text is never shown.
export const useReport = () => {
  const platform = usePlatform()
  const { $gettext, interpolate } = useGettext()

  const describe = (e: unknown): ErrorText => {
    const message = describeError(e, $gettext)
    if (e instanceof ApiError && e.code === 'rate_limited' && e.retryAfter !== undefined) {
      return {
        message,
        detail: interpolate($gettext('Try again in %{seconds} seconds.'), { seconds: e.retryAfter }, true)
      }
    }
    return { message }
  }

  const notifyError = (e: unknown) => {
    const t = describe(e)
    platform.notify('error', t.message, t.detail)
  }

  return { describe, notifyError }
}
