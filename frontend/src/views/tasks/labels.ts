import type { JobState } from '../../api/types'

type Gettext = (msgid: string) => string

export interface StateLabel {
  known: boolean
  icon: string
  label: string
}

const STATES: Record<string, { icon: string; label: (t: Gettext) => string }> = {
  ['queued' satisfies JobState]: { icon: 'time', label: (t) => t('Waiting') },
  ['running' satisfies JobState]: { icon: 'loader-4', label: (t) => t('Working') },
  ['waiting_approval' satisfies JobState]: { icon: 'hourglass', label: (t) => t('Needs your approval') },
  ['succeeded' satisfies JobState]: { icon: 'checkbox-circle', label: (t) => t('Done') },
  ['failed' satisfies JobState]: { icon: 'error-warning', label: (t) => t('Failed') },
  ['cancelled' satisfies JobState]: { icon: 'close-circle', label: (t) => t('Cancelled') }
}

// stateLabel is the text label and icon of a job state; an unknown state
// gets a generic label (show the raw value next to it as text).
export const stateLabel = (state: string, t: Gettext): StateLabel =>
  Object.hasOwn(STATES, state)
    ? { known: true, icon: STATES[state].icon, label: STATES[state].label(t) }
    : { known: false, icon: 'question', label: t('Unknown state:') }

// filterLabel is the label of a list filter (a job state, or '' for all).
export const filterLabel = (state: string, t: Gettext): string => {
  switch (state) {
    case '':
      return t('All tasks')
    case 'queued' satisfies JobState:
      return t('Waiting')
    case 'running' satisfies JobState:
      return t('Working')
    case 'waiting_approval' satisfies JobState:
      return t('Needs your approval')
    case 'succeeded' satisfies JobState:
      return t('Done')
    case 'failed' satisfies JobState:
      return t('Failed')
    case 'cancelled' satisfies JobState:
      return t('Cancelled')
  }
  return state
}

// triggerLabel names what started a job; an unknown trigger kind gets
// null (show the raw value as text).
export const triggerLabel = (kind: string, t: Gettext): string | null => {
  switch (kind) {
    case 'chat':
      return t('Chat question')
    case 'schedule':
      return t('Scheduled task')
  }
  return null
}

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

// stepKindLabel names a timeline entry's kind; null for an unknown one.
export const stepKindLabel = (kind: string, t: Gettext): string | null => {
  switch (kind) {
    case 'llm':
      return t('Model turn')
    case 'tool':
      return t('Tool call')
  }
  return null
}
