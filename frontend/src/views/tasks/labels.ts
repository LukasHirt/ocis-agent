import type { Job, JobState } from '../../api/types'

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

// conversationOf is the id of the conversation a chat job answers, or
// null for any other job.
export const conversationOf = (job: Pick<Job, 'trigger_kind' | 'trigger_ref'>): string | null =>
  job.trigger_kind === 'chat' && typeof job.trigger_ref === 'string' && job.trigger_ref !== '' ? job.trigger_ref : null

// tokenCount is a token count from the API, or 0 when it is not one.
export const tokenCount = (n: unknown): number => (typeof n === 'number' && Number.isSafeInteger(n) && n > 0 ? n : 0)

// errorText is shared with the chat screen (a failed question, a notice).
export { errorText } from '../../util/reasons'

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
