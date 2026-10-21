type Gettext = (msgid: string) => string

// The audit event kinds the sidecar writes, each with its label (the
// server's kind filter accepts exactly these). Every msgid is a literal so
// it can be extracted for translation.
const KINDS: Record<string, (t: Gettext) => string> = {
  approval_requested: (t) => t('Approval requested'),
  approval_decided: (t) => t('Approval decided'),
  approval_expired: (t) => t('Approval expired'),
  approval_acknowledged: (t) => t('Outcome check confirmed by you'),
  tool_call: (t) => t('Tool used'),
  action_executed: (t) => t('Action carried out'),
  policy_denied: (t) => t('Action refused by policy'),
  tampered: (t) => t('Changed action refused'),
  memory_changed: (t) => t('Memory changed by the agent'),
  memory_edited: (t) => t('Memory edited by you'),
  job_finished: (t) => t('Task ended'),
  job_cancelled: (t) => t('Task cancelled'),
  chat_message: (t) => t('Chat question'),
  conversation_deleted: (t) => t('Conversation deleted'),
  agent_paused: (t) => t('Agent paused'),
  agent_resumed: (t) => t('Agent resumed'),
  agent_dormant: (t) => t('Agent paused after inactivity'),
  policy_changed: (t) => t('Settings changed'),
  token_created: (t) => t('Agent access created'),
  token_renewed: (t) => t('Agent access renewed'),
  token_deleted: (t) => t('Agent access token deleted'),
  token_label_closed: (t) => t('Unused access token closed'),
  token_revoked_all: (t) => t('All app tokens revoked'),
  token_invalid: (t) => t('Agent access stopped working'),
  token_expired: (t) => t('Agent access expired'),
  token_anomaly: (t) => t('Unknown access token found'),
  unenrolled: (t) => t('Agent turned off')
}

export const KNOWN_KINDS: readonly string[] = Object.freeze(Object.keys(KINDS))

// The kinds whose detail is a line diff of a memory file.
export const DIFF_KINDS: readonly string[] = ['memory_changed', 'memory_edited']

// kindLabel is the label of an event kind, or null for a kind this client
// does not know (show the raw value as text).
export const kindLabel = (kind: string, t: Gettext): string | null =>
  Object.hasOwn(KINDS, kind) ? KINDS[kind](t) : null

// credLabel names who acted: the user (with their sign-in) or the agent
// (with its app token); null for any other value. The runner's own events
// carry no credentials ('') and name nobody.
export const credLabel = (cred: string, t: Gettext): string | null => {
  switch (cred) {
    case 'oidc':
      return t('You')
    case 'app':
      return t('The agent')
  }
  return null
}

export interface DiffLine {
  kind: 'add' | 'del' | 'note' | 'context'
  text: string
}

// diffLines splits a diff into lines classed by their first character
// (+ added, - removed, ~ a note); the text stays as it is.
export const diffLines = (diff: string): DiffLine[] => {
  const lines = diff.endsWith('\n') ? diff.slice(0, -1).split('\n') : diff.split('\n')
  return lines.map((text) => ({
    kind: text.startsWith('+') ? 'add' : text.startsWith('-') ? 'del' : text.startsWith('~') ? 'note' : 'context',
    text
  }))
}

// memoryEdit splits the detail of a memory_edited event ("name=<file>;
// diff=<diff>") into the file name and the diff; null if it has another
// shape.
export const memoryEdit = (detail: string): { name: string; diff: string } | null => {
  const m = /^name=([^;\n]*); diff=/.exec(detail)
  return m ? { name: m[1], diff: detail.slice(m[0].length) } : null
}
