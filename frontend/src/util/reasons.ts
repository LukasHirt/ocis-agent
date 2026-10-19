// The runner's fixed texts (Reason* in backend/internal/agent), as job
// errors and as the chat's system messages.

type Gettext = (msgid: string) => string

// The runner's fixed reasons (the job's error field), each with a msgid so
// it can be translated. The server text itself is the key.
const REASONS: Record<string, (t: Gettext) => string> = {
  'The agent stopped because this task needed more steps than allowed.': (t) =>
    t('The agent stopped because this task needed more steps than allowed.'),
  'The agent stopped because this task was restarted too many times.': (t) =>
    t('The agent stopped because this task was restarted too many times.'),
  'The agent stopped because it kept repeating the same action.': (t) =>
    t('The agent stopped because it kept repeating the same action.'),
  'The agent stopped because the usage budget is used up.': (t) =>
    t('The agent stopped because the usage budget is used up.'),
  'The agent is switched off by the operator.': (t) => t('The agent is switched off by the operator.'),
  'The agent could not access your files with its stored credentials.': (t) =>
    t('The agent could not access your files with its stored credentials.'),
  'The language model provider refused the configured credentials.': (t) =>
    t('The language model provider refused the configured credentials.'),
  'The language model provider rejected the request.': (t) => t('The language model provider rejected the request.'),
  'The language model returned an unusable answer.': (t) => t('The language model returned an unusable answer.'),
  'The language model answer was cut off before it was complete.': (t) =>
    t('The language model answer was cut off before it was complete.'),
  'The task grew too large to continue.': (t) => t('The task grew too large to continue.'),
  'The agent stopped because of an internal error.': (t) => t('The agent stopped because of an internal error.')
}

// errorText is the translated text of a job's error, or null when the
// error is not one of the known reasons (show it as untrusted text then).
export const errorText = (error: string, t: Gettext): string | null =>
  Object.hasOwn(REASONS, error) ? REASONS[error](t) : null
