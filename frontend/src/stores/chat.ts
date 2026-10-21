import { defineStore } from 'pinia'
import { computed, reactive, ref, shallowRef, watch } from 'vue'
import { useAgentStore, type SessionHandle, type StoreEvent } from './agent'
import { ApiError, describeError, isAbortError } from '../api/errors'
import { coalesce } from '../util/coalesce'
import { byId, listOf, mergeHead } from '../util/lists'
import type { Approval, Conversation, JobState, Message, PartialState, PauseReason, Sent } from '../api/types'
import { MAX_MESSAGE_BYTES, MAX_TITLE_CHARS, checkMessage, checkTitle, type TextProblem } from '../util/chatText'
import { formatDuration } from '../util/format'

// Page sizes (the server caps chat pages at 100).
export const CONVERSATION_PAGE = 50
export const MESSAGE_PAGE = 50
export const JOB_APPROVALS_LIMIT = 20
// The server's per-user pause between two sends; a refused send uses it too.
export const SEND_COOLDOWN_MS = 1_000
// A Retry-After blocks sending locally for at most this long; after that
// the server decides again (a budget raised in the settings counts at once).
export const LOCAL_BLOCK_MAX_MS = 60_000

// Why the agent will not answer a message right away, from the 202.
export type AgentNotice = 'paused' | 'killed' | 'credentials'

export interface SendError {
  code: string // the server's code, or empty, too_long, control, cooldown
  message: string // a fixed text
  retryAfter?: number // seconds, as the server said
}

export interface JobApprovals {
  items: Approval[]
  more: boolean // more pending approvals than were read
  error: string | null
}

// The states in which a question is still unanswered.
const LIVE_STATES: readonly string[] = [
  'queued' satisfies JobState,
  'running' satisfies JobState,
  'waiting_approval' satisfies JobState
]

export const isLiveJob = (state: string | undefined): boolean => state !== undefined && LIVE_STATES.includes(state)

// The unfinished answer of a running question: its text so far (untrusted)
// and whether the model is thinking (no text yet) or writing.
export interface PartialAnswer {
  text: string
  state: 'thinking' | 'writing'
}

const PARTIAL_STATES: readonly string[] = ['thinking' satisfies PartialState, 'writing' satisfies PartialState]

// partialOf is the unfinished answer to show under a question: only while
// its job is running and says what it is doing (like the server, which
// blanks it otherwise; a stale one is never shown as a result).
const partialOf = (m: Message): PartialAnswer | null => {
  const j = m.job
  if (m.role !== 'user' || !j || j.state !== ('running' satisfies JobState)) return null
  const state = String(j.partial_state ?? '')
  if (!PARTIAL_STATES.includes(state)) return null
  return { text: typeof j.partial === 'string' ? j.partial : '', state: state as PartialAnswer['state'] }
}

const noticeOf = (a: Sent['agent'] | undefined): AgentNotice | undefined => {
  if (!a || typeof a !== 'object') return undefined
  if (a.kill_switch === true) return 'killed'
  if (a.paused !== true) return undefined
  const reason = String(a.pause_reason ?? '')
  const credential: readonly string[] = [
    'credential_invalid' satisfies PauseReason,
    'credential_revoked' satisfies PauseReason,
    'credential_expired' satisfies PauseReason
  ]
  return credential.includes(reason) ? 'credentials' : 'paused'
}

// useChatStore holds the chat screen's data: the conversation list, the
// open conversation's messages and the send state. It refreshes on the
// agent store's events only while attached (the chat screen is mounted).
// Errors are fixed texts; nothing is optimistic: a sent message shows when
// the server lists it.
export const useChatStore = defineStore('ocis-agent-chat', () => {
  const agent = useAgentStore()

  const conversations = shallowRef<Conversation[]>([])
  const conversationsCursor = ref<string | null>(null)
  const listBusy = ref(0) // list reads in flight
  const conversationsLoading = computed(() => listBusy.value > 0)
  const conversationsLoaded = ref(false)
  const conversationsError = ref<string | null>(null)

  const current = ref<string | null>(null)
  const detail = shallowRef<Conversation | null>(null) // the open one, if not listed
  const messages = shallowRef<Message[]>([]) // newest first, as the API lists them
  const olderCursor = ref<string | null>(null)
  const messagesLoading = ref(false)
  const messagesLoaded = ref(false)
  const messagesError = ref<string | null>(null)
  const olderLoading = ref(false)
  const olderError = ref<string | null>(null)
  const missing = ref(false) // the open conversation does not exist (any more)

  // the conversations with a send in flight: a send outlives switching
  // to (or deleting) its conversation, and blocks only its own composer
  const sendingIn = reactive(new Set<string>())
  const sending = computed(() => current.value !== null && sendingIn.has(current.value))
  const sendError = shallowRef<SendError | null>(null)
  const blockedUntil = ref(0)
  const needsEnrol = ref(false)
  const actionError = ref<string | null>(null)
  const draftByConversation = reactive<Record<string, string>>({})
  const notices = reactive<Record<string, AgentNotice>>({})
  const jobApprovals = reactive<Record<string, JobApprovals>>({})

  // the chronological thread (oldest first)
  const thread = computed(() => [...messages.value].reverse())
  // the unfinished answers by question id; a question whose answer is
  // listed already shows none (the answer replaces it in the same read)
  const partials = computed(() => {
    const answered = new Set<string>()
    for (const m of messages.value) if (m.role !== 'user' && m.job_id) answered.add(m.job_id)
    const out: Record<string, PartialAnswer> = {}
    for (const m of messages.value) {
      const p = partialOf(m)
      if (p && !answered.has(m.job!.id)) out[m.id] = p
    }
    return out
  })
  const currentConversation = computed(
    () => conversations.value.find((c) => c.id === current.value) ?? detail.value ?? null
  )

  let bound: SessionHandle | null = null // the session the data belongs to
  // Answers are dropped when their generation moved on: sessionGen grows
  // on reset only (the list and the sends), threadGen also on every open
  // and close (the open conversation's messages and approvals).
  let sessionGen = 0
  let threadGen = 0

  // session returns the running session, resetting the data first if it
  // belongs to an earlier one.
  const session = (): SessionHandle | null => {
    const s = agent.session()
    if (s !== bound) {
      if (bound !== null) reset()
      bound = s
    }
    return s
  }

  // syncSession drops the data of an earlier session (call it before
  // relying on what the store holds, such as the open conversation).
  const syncSession = (): void => {
    session()
  }

  const describe = (s: SessionHandle, e: unknown) => describeError(e, s.gettext)
  const fmtSeconds = (secs: number) => formatDuration(Math.max(1, secs) * 1000)

  // --- conversations

  const applyConversations = (head: { items: Conversation[]; next: string | null }) => {
    const r = mergeHead(conversations.value, head, conversationsCursor.value)
    conversations.value = r.items
    conversationsCursor.value = r.cursor
  }

  // replaceNext makes the next first-page read replace the list instead of
  // merging (after the cursor became unknown)
  let replaceNext = false

  const conversationsRefresh = coalesce(async () => {
    const s = session()
    if (!s) return
    const g = sessionGen
    const replace = replaceNext
    replaceNext = false
    listBusy.value++
    try {
      const v = await s.client.listConversations({ limit: CONVERSATION_PAGE }, s.signal)
      if (g !== sessionGen) return
      const head = listOf<Conversation>(v, 'conversations')
      if (replace) {
        conversations.value = head.items
        conversationsCursor.value = head.next
      } else {
        applyConversations(head)
      }
      conversationsError.value = null
      conversationsLoaded.value = true
    } catch (e) {
      if (g !== sessionGen || isAbortError(e)) return
      conversationsError.value = describe(s, e)
      agent.reportError(e)
    } finally {
      if (g === sessionGen) listBusy.value--
    }
  })

  // loadConversations reads the first page (merged with the pages already
  // read, see mergeHead).
  const loadConversations = (): Promise<void> => conversationsRefresh()

  let moreInflight: Promise<void> | null = null
  const loadMoreConversations = (): Promise<void> => {
    if (moreInflight) return moreInflight
    const s = session()
    const cursor = conversationsCursor.value
    if (!s || cursor === null) return Promise.resolve()
    const g = sessionGen
    listBusy.value++
    let stale = false
    const p = (async () => {
      try {
        const v = await s.client.listConversations({ limit: CONVERSATION_PAGE, cursor }, s.signal)
        if (g !== sessionGen) return
        const page = listOf<Conversation>(v, 'conversations')
        const seen = byId(conversations.value)
        conversations.value = [...conversations.value, ...page.items.filter((c) => !seen.has(c.id))]
        conversationsCursor.value = page.next
        conversationsError.value = null
      } catch (e) {
        if (g !== sessionGen || isAbortError(e)) return
        // the cursor's conversation is gone (or moved): read from the top
        if (e instanceof ApiError && e.status === 400) stale = true
        else {
          conversationsError.value = describe(s, e)
          agent.reportError(e)
        }
      } finally {
        if (g === sessionGen) listBusy.value--
        moreInflight = null
      }
      if (stale && g === sessionGen) {
        replaceNext = true
        await conversationsRefresh()
      }
    })()
    moreInflight = p
    return p
  }

  // dropConversation takes a conversation that is gone off the list; its
  // draft stays (the user may still want the text) unless the user deleted
  // it here.
  const dropConversation = (id: string, forgetDraft = false) => {
    conversations.value = conversations.value.filter((c) => c.id !== id)
    if (forgetDraft) delete draftByConversation[id]
  }

  const replaceConversation = (c: Conversation) => {
    conversations.value = conversations.value.map((x) => (x.id === c.id ? c : x))
    if (detail.value?.id === c.id) detail.value = c
  }

  // --- the open conversation

  const clearThread = () => {
    messages.value = []
    olderCursor.value = null
    messagesLoaded.value = false
    messagesLoading.value = false
    messagesError.value = null
    olderLoading.value = false
    olderError.value = null
    missing.value = false
    detail.value = null
    for (const k of Object.keys(jobApprovals)) delete jobApprovals[k]
  }

  const approvalsInflight = new Map<string, Promise<void>>()

  // loadJobApprovals reads the pending approvals of the jobs that wait for
  // one, for the inline list; the others are forgotten.
  const loadJobApprovals = (s: SessionHandle, g: number) => {
    const waiting = new Set<string>()
    for (const m of messages.value) {
      if (m.job && m.job.state === ('waiting_approval' satisfies JobState)) waiting.add(m.job.id)
    }
    for (const k of Object.keys(jobApprovals)) if (!waiting.has(k)) delete jobApprovals[k]
    return Promise.all(
      [...waiting].map((jobId) => {
        const running = approvalsInflight.get(jobId)
        if (running) return running
        const p = s.client
          .listApprovals({ jobId, limit: JOB_APPROVALS_LIMIT }, s.signal)
          .then(
            (v) => {
              if (g !== threadGen) return
              const page = listOf<Approval>(v, 'approvals')
              jobApprovals[jobId] = { items: page.items, more: page.next !== null, error: null }
            },
            (e: unknown) => {
              if (g !== threadGen || isAbortError(e)) return
              const prev = jobApprovals[jobId]
              jobApprovals[jobId] = { items: prev?.items ?? [], more: prev?.more ?? false, error: describe(s, e) }
            }
          )
          .finally(() => approvalsInflight.delete(jobId))
        approvalsInflight.set(jobId, p)
        return p
      })
    ).then(() => undefined)
  }

  const threadRefresh = coalesce(async () => {
    const s = session()
    const id = current.value
    if (!s || id === null) return
    const g = threadGen
    messagesLoading.value = true
    try {
      const v = await s.client.listMessages(id, { limit: MESSAGE_PAGE }, s.signal)
      if (g !== threadGen) return
      const r = mergeHead(messages.value, listOf<Message>(v, 'messages'), olderCursor.value)
      messages.value = r.items
      olderCursor.value = r.cursor
      messagesError.value = null
      messagesLoaded.value = true
      missing.value = false
    } catch (e) {
      if (g !== threadGen || isAbortError(e)) return
      if (e instanceof ApiError && e.status === 404) {
        missing.value = true
        messages.value = []
        olderCursor.value = null
        dropConversation(id)
        messagesError.value = null
      } else {
        messagesError.value = describe(s, e)
        agent.reportError(e)
      }
      return
    } finally {
      if (g === threadGen) messagesLoading.value = false
    }
    await loadJobApprovals(s, g)
  })

  // refreshOpen reads the first page of the open conversation again (and
  // the approvals of its waiting questions).
  const refreshOpen = (): Promise<void> => threadRefresh()

  // openConversation shows the conversation with this id; the answers for
  // the one shown before are dropped.
  const openConversation = async (id: string): Promise<void> => {
    if (!session()) return
    threadGen++
    threadRefresh.clear()
    approvalsInflight.clear()
    clearThread()
    current.value = id
    sendError.value = null
    actionError.value = null
    const g = threadGen
    const s = session()!
    if (!conversations.value.some((c) => c.id === id)) {
      // opened by its address: read its title
      s.client.getConversation(id, s.signal).then(
        (c) => {
          if (g === threadGen && c && typeof c.id === 'string') detail.value = c
        },
        () => {} // the messages tell whether it exists
      )
    }
    await threadRefresh()
  }

  const closeConversation = () => {
    threadGen++
    threadRefresh.clear()
    approvalsInflight.clear()
    clearThread()
    current.value = null
    sendError.value = null
  }

  const loadOlder = async (): Promise<void> => {
    const s = session()
    const id = current.value
    const cursor = olderCursor.value
    if (!s || id === null || cursor === null || olderLoading.value) return
    const g = threadGen
    olderLoading.value = true
    olderError.value = null
    try {
      const v = await s.client.listMessages(id, { limit: MESSAGE_PAGE, cursor }, s.signal)
      if (g !== threadGen) return
      const page = listOf<Message>(v, 'messages')
      const seen = byId(messages.value)
      messages.value = [...messages.value, ...page.items.filter((m) => !seen.has(m.id))]
      olderCursor.value = page.next
    } catch (e) {
      if (g !== threadGen || isAbortError(e)) return
      olderError.value = describe(s, e)
    } finally {
      if (g === threadGen) olderLoading.value = false
    }
  }

  // --- actions

  const textProblem = (s: SessionHandle, p: TextProblem): string => {
    switch (p) {
      case 'empty':
        return s.gettext('Write a message first.')
      case 'too_long':
        return s.gettext('The message is longer than %{max} bytes. Shorten it.').replace(
          '%{max}',
          String(MAX_MESSAGE_BYTES)
        )
      case 'control':
        return s.gettext('The message contains control characters, which cannot be sent. Remove them.')
    }
  }

  const sendErrorOf = (s: SessionHandle, e: unknown): SendError => {
    if (!(e instanceof ApiError)) return { code: 'internal', message: describe(s, e) }
    const retry = e.retryAfter !== undefined ? { retryAfter: e.retryAfter } : {}
    const base = { code: e.code, ...retry }
    const t = s.gettext
    if (e.status === 429) {
      switch (e.code) {
        case 'too_many_pending':
          return { ...base, message: t('You have too many questions waiting. Wait for an answer, then try again.') }
        case 'budget_exhausted':
          return { ...base, message: describe(s, e) }
        case 'limit_reached':
          return {
            ...base,
            message:
              e.retryAfter !== undefined
                ? t("You have reached today's limit of questions. You can ask again in %{time}.").replace(
                    '%{time}',
                    fmtSeconds(e.retryAfter)
                  )
                : t("You have reached today's limit of questions. Try again tomorrow.")
          }
        case 'rate_limited':
          return {
            ...base,
            message:
              e.retryAfter !== undefined
                ? t('You are sending messages too quickly. Try again in %{time}.').replace(
                    '%{time}',
                    fmtSeconds(e.retryAfter)
                  )
                : describe(s, e)
          }
      }
    }
    if (e.status === 404) return { ...base, message: t('This conversation no longer exists.') }
    return { ...base, message: describe(s, e) }
  }

  // send sends text to the open conversation. It refuses while a send to
  // it is in flight or the cooldown runs, and text the server would refuse.
  // After the 202 the draft is cleared and the thread read again; on a
  // refusal the draft stays and sendError has the fixed text. A send
  // outlives switching away: its draft is still cleared on the 202, but its
  // refusal is not shown in another conversation.
  const send = async (text: string): Promise<boolean> => {
    const s = session()
    const id = current.value
    if (!s || id === null || sendingIn.has(id)) return false
    const problem = checkMessage(text)
    if (problem) {
      sendError.value = { code: problem, message: textProblem(s, problem) }
      return false
    }
    const now = s.platform.now()
    if (now < blockedUntil.value) {
      const secs = Math.ceil((blockedUntil.value - now) / 1000)
      sendError.value = {
        code: 'cooldown',
        message: s.gettext('Wait %{time} before sending again.').replace('%{time}', fmtSeconds(secs))
      }
      return false
    }
    const g = sessionGen
    sendingIn.add(id)
    sendError.value = null
    try {
      const res = await s.client.sendMessage(id, text, s.signal)
      if (g !== sessionGen) return false
      blockedUntil.value = s.platform.now() + SEND_COOLDOWN_MS
      delete draftByConversation[id]
      needsEnrol.value = false
      const notice = noticeOf(res?.agent)
      const mid = res?.message?.id
      if (notice && typeof mid === 'string') notices[mid] = notice
      await Promise.all([current.value === id ? threadRefresh() : undefined, conversationsRefresh()])
      return true
    } catch (e) {
      if (g !== sessionGen || isAbortError(e)) return false
      const err = sendErrorOf(s, e)
      const wait = e instanceof ApiError && e.status === 429 && e.retryAfter !== undefined ? e.retryAfter * 1000 : 0
      // the server's word on long refusals (the daily cap, the budget) can
      // change sooner (settings): hold at most LOCAL_BLOCK_MAX_MS locally
      blockedUntil.value = s.platform.now() + Math.min(Math.max(SEND_COOLDOWN_MS, wait), LOCAL_BLOCK_MAX_MS)
      if (err.code === 'not_enrolled') needsEnrol.value = true
      const open = current.value === id
      if (e instanceof ApiError && e.status === 404) {
        if (open) missing.value = true
        dropConversation(id)
      }
      if (open) sendError.value = err
      return false
    } finally {
      if (g === sessionGen) sendingIn.delete(id)
    }
  }

  // createConversation creates one (untitled unless given a title) at the
  // top of the list; null after a refusal (actionError says why).
  const createConversation = async (title?: string): Promise<Conversation | null> => {
    const s = session()
    if (!s) return null
    actionError.value = null
    try {
      const c = await s.client.createConversation(title, s.signal)
      if (bound !== s || !c || typeof c.id !== 'string') return null
      conversations.value = [c, ...conversations.value.filter((x) => x.id !== c.id)]
      needsEnrol.value = false
      return c
    } catch (e) {
      if (bound !== s || isAbortError(e)) return null
      if (e instanceof ApiError && e.status === 409 && e.code === 'limit_reached') {
        actionError.value = s.gettext(
          'You have 200 conversations, the most there can be. Delete one to start a new one.'
        )
      } else {
        if (e instanceof ApiError && e.code === 'not_enrolled') needsEnrol.value = true
        actionError.value = describe(s, e)
      }
      return null
    }
  }

  const titleProblem = (s: SessionHandle, p: TextProblem): string => {
    switch (p) {
      case 'empty':
        return s.gettext('Enter a title.')
      case 'too_long':
        return s.gettext('The title can have at most %{max} characters.').replace('%{max}', String(MAX_TITLE_CHARS))
      case 'control':
        return s.gettext('The title cannot contain control or text direction characters.')
    }
  }

  // checkTitleText returns the fixed text of what is wrong with a title the
  // user typed (trimmed), or null.
  const titleError = (title: string): string | null => {
    const s = session()
    if (!s) return null
    const p = checkTitle(title.trim())
    return p ? titleProblem(s, p) : null
  }

  const rename = async (id: string, title: string): Promise<boolean> => {
    const s = session()
    if (!s) return false
    const t = title.trim()
    const p = checkTitle(t)
    if (p) {
      actionError.value = titleProblem(s, p)
      return false
    }
    actionError.value = null
    try {
      const c = await s.client.renameConversation(id, t, s.signal)
      if (bound !== s) return false
      if (c && typeof c.id === 'string') replaceConversation(c)
      return true
    } catch (e) {
      if (bound !== s || isAbortError(e)) return false
      if (e instanceof ApiError && e.status === 404) dropConversation(id)
      actionError.value = describe(s, e)
      return false
    }
  }

  // remove deletes a conversation after the user confirmed it; one that is
  // already gone counts as deleted.
  const remove = async (id: string): Promise<boolean> => {
    const s = session()
    if (!s) return false
    actionError.value = null
    const ok = await s.platform.confirm({
      title: s.gettext('Delete this conversation?'),
      message: s.gettext(
        'Its messages are deleted. Questions in it that are still waiting or being answered are cancelled. This cannot be undone.'
      ),
      confirmText: s.gettext('Delete'),
      danger: true
    })
    if (!ok || bound !== s) return false
    try {
      await s.client.deleteConversation(id, s.signal)
    } catch (e) {
      if (bound !== s || isAbortError(e)) return false
      if (!(e instanceof ApiError && e.status === 404)) {
        actionError.value = describe(s, e)
        return false
      }
    }
    if (bound !== s) return false
    dropConversation(id, true)
    if (current.value === id) closeConversation()
    // its live jobs were cancelled: their approvals are gone
    void agent.refreshApprovals()
    void agent.refreshStatus()
    return true
  }

  // enrol enables the agent after a not_enrolled refusal.
  const enrol = async (): Promise<boolean> => {
    const s = session()
    if (!s) return false
    try {
      await agent.enrol()
      needsEnrol.value = false
      actionError.value = null
      sendError.value = null
      return true
    } catch (e) {
      if (bound === s && !isAbortError(e)) actionError.value = describe(s, e)
      return false
    }
  }

  // --- live updates

  const hasLiveJob = () => messages.value.some((m) => isLiveJob(m.job?.state))
  const hasWaitingJob = () =>
    messages.value.some((m) => m.job?.state === ('waiting_approval' satisfies JobState))

  // unblock lets the next send ask the server again (the settings or the
  // budget changed, so a budget refusal may no longer hold)
  const unblock = () => {
    blockedUntil.value = 0
  }
  watch(
    () => agent.status?.budget?.daily_tokens,
    (now, before) => {
      if (before !== undefined && now !== before) unblock()
    }
  )

  const onEvent = async (e: StoreEvent) => {
    if (e.type === 'reset') {
      reset()
      return
    }
    if (!session()) return
    const work: Promise<void>[] = []
    if (e.type === 'refresh') {
      work.push(conversationsRefresh())
      if (current.value !== null) work.push(threadRefresh())
    } else {
      const msgs = e.kinds.includes('messages')
      if (e.kinds.includes('settings')) unblock()
      if (msgs) work.push(conversationsRefresh())
      if (current.value !== null && (msgs || (e.kinds.includes('jobs') && hasLiveJob()))) {
        work.push(threadRefresh())
      } else if (current.value !== null && e.kinds.includes('activity') && hasWaitingJob()) {
        // a decided approval moves the activity mark, not always the job
        const s = session()
        if (s) work.push(loadJobApprovals(s, threadGen))
      }
    }
    await Promise.all(work)
  }

  // attach subscribes to the agent store's events while the chat screen is
  // shown; it returns the function that detaches again.
  const attach = (): (() => void) => {
    session() // drop data of an earlier session
    return agent.subscribe(onEvent)
  }

  // reset forgets everything, drafts too: a later session may be another
  // user's.
  const reset = () => {
    sessionGen++
    threadGen++
    conversationsRefresh.clear()
    threadRefresh.clear()
    approvalsInflight.clear()
    moreInflight = null
    conversations.value = []
    conversationsCursor.value = null
    listBusy.value = 0
    replaceNext = false
    conversationsLoaded.value = false
    conversationsError.value = null
    clearThread()
    current.value = null
    sendingIn.clear()
    sendError.value = null
    blockedUntil.value = 0
    needsEnrol.value = false
    actionError.value = null
    for (const k of Object.keys(draftByConversation)) delete draftByConversation[k]
    for (const k of Object.keys(notices)) delete notices[k]
    bound = agent.session()
  }

  // reloadJobApprovals rereads the pending approvals of the waiting jobs, after
  // an inline decision.
  const reloadJobApprovals = (): Promise<unknown> => {
    const s = session()
    return s ? loadJobApprovals(s, threadGen) : Promise.resolve()
  }

  return {
    conversations,
    conversationsCursor,
    conversationsLoading,
    conversationsLoaded,
    conversationsError,
    current,
    currentConversation,
    messages,
    thread,
    partials,
    olderCursor,
    messagesLoading,
    messagesLoaded,
    messagesError,
    olderLoading,
    olderError,
    missing,
    sending,
    sendError,
    blockedUntil,
    needsEnrol,
    actionError,
    draftByConversation,
    notices,
    jobApprovals,
    attach,
    syncSession,
    loadConversations,
    loadMoreConversations,
    openConversation,
    closeConversation,
    createConversation,
    titleError,
    rename,
    remove,
    enrol,
    loadOlder,
    send,
    refreshOpen,
    reloadJobApprovals,
    reset
  }
})
