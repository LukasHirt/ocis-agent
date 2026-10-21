import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { useAgentStore, type SessionHandle, type StoreEvent } from './agent'
import { ApiError, describeError, isAbortError } from '../api/errors'
import type { MemoryFile, MemoryFileInfo } from '../api/types'
import { coalesce } from '../util/coalesce'
import {
  CORE_NAMES,
  checkContent,
  describeContentProblem,
  isCoreName,
  isMemoryName,
  memoryOrder,
  type ContentProblem
} from '../util/memoryText'

// Conflict is shown after a save found the file changed (412): current is
// the version on the server now, or null when the file was deleted.
export interface Conflict {
  current: MemoryFile | null
}

const isFile = (v: unknown): v is MemoryFile => {
  const f = v as Partial<MemoryFile> | null
  return (
    typeof f === 'object' &&
    f !== null &&
    typeof f.name === 'string' &&
    typeof f.content === 'string' &&
    typeof f.etag === 'string' &&
    f.etag !== ''
  )
}

const fileOf = (v: unknown): MemoryFile => {
  if (!isFile(v)) throw new ApiError(502, 'internal')
  // a missing flag counts as the restrictive value
  return { name: v.name, content: v.content, etag: v.etag, truncated: v.truncated !== false, editable: v.editable === true }
}

const infosOf = (v: unknown): MemoryFileInfo[] => {
  const files = (v as { files?: unknown } | null)?.files
  if (!Array.isArray(files)) throw new ApiError(502, 'internal')
  return files
    .filter(
      (f): f is MemoryFileInfo =>
        typeof f === 'object' && f !== null && typeof f.name === 'string' && isMemoryName(f.name)
    )
    .sort((a, b) => memoryOrder(a.name, b.name))
}

// A textarea turns every CRLF and lone CR into LF. The draft is kept with
// LF; a file that used CRLF throughout is saved with CRLF again, a file
// with mixed line endings is saved with LF (the editor says so).
type Eol = 'lf' | 'crlf' | 'mixed'

const normalise = (s: string): string => s.replace(/\r\n?/g, '\n')

const eolOf = (s: string): Eol => {
  const crlf = s.includes('\r\n')
  const loneCR = /\r(?!\n)/.test(s)
  const loneLF = /(?<!\r)\n/.test(s)
  if (loneCR || (crlf && loneLF)) return 'mixed'
  return crlf ? 'crlf' : 'lf'
}

const is = (e: unknown, status: number) => e instanceof ApiError && e.status === status

// useMemoryStore holds the memory screen's data: the file list, the open
// file and its editor. The editor never loses the user's draft: a save
// sends the etag exactly as the file was read, a changed file (412) shows
// both versions, and live changes never replace a changed draft. It
// refreshes on the agent store's events only while attached (the memory
// screen is mounted), and only what was read before.
export const useMemoryStore = defineStore('ocis-agent-memory', () => {
  const agent = useAgentStore()

  const files = shallowRef<MemoryFileInfo[]>([])
  const listLoading = ref(false)
  const listLoaded = ref(false)
  const listError = ref<string | null>(null)

  const openName = ref<string | null>(null)
  const file = shallowRef<MemoryFile | null>(null)
  const fileLoading = ref(false)
  const fileError = ref<string | null>(null)
  const fileMissing = ref(false)

  const editing = ref(false)
  const draft = ref('')
  const base = shallowRef<MemoryFile | null>(null) // the version the draft started from
  const saving = ref(false)
  const saveError = ref<string | null>(null)
  const conflict = shallowRef<Conflict | null>(null)
  const serverChanged = ref(false)
  const eol = ref<Eol>('lf')

  const creating = ref(false)
  const createError = ref<string | null>(null)

  const coreFiles = computed(() => files.value.filter((f) => isCoreName(f.name)))
  const notes = computed(() => files.value.filter((f) => !isCoreName(f.name)))
  const missingCore = computed(() => CORE_NAMES.filter((n) => !files.value.some((f) => f.name === n)))
  const canEdit = computed(() => file.value !== null && file.value.editable && !file.value.truncated)
  const dirty = computed(() => editing.value && base.value !== null && draft.value !== normalise(base.value.content))
  const mixedLineEndings = computed(() => editing.value && eol.value === 'mixed')
  // toSave is the draft as it is saved: with the file's CRLF line endings
  const toSave = computed(() => (eol.value === 'crlf' ? draft.value.replace(/\n/g, '\r\n') : draft.value))
  const contentProblem = computed(() => checkContent(toSave.value))

  let bound: SessionHandle | null = null
  let listGen = 0
  let fileGen = 0 // grows on reset and every open
  let writeGen = 0 // grows on every write: reads that started before it are dropped
  let listed = false
  // the list etag of the open file already checked against the file: the
  // list and a read may spell an etag differently, so a mismatch is
  // checked once by reading the file
  let checkedEtag: string | null = null

  const session = (): SessionHandle | null => {
    const s = agent.session()
    if (s !== bound) {
      if (bound !== null) reset()
      bound = s
    }
    return s
  }

  const describe = (s: SessionHandle, e: unknown) => describeError(e, s.gettext)

  const contentMessage = (s: SessionHandle, p: ContentProblem): string => describeContentProblem(p, s.gettext)

  // writeMessage maps a failed write to its fixed text.
  const writeMessage = (s: SessionHandle, e: unknown): string => {
    if (e instanceof ApiError && e.code === 'too_large') return contentMessage(s, 'too_large')
    return describe(s, e)
  }

  // --- the list

  const listRefresh = coalesce(async () => {
    const s = session()
    if (!s) return
    listed = true
    const g = listGen
    listLoading.value = true
    try {
      const v = await s.client.listMemory(s.signal)
      if (g !== listGen) return
      files.value = infosOf(v)
      listError.value = null
      listLoaded.value = true
    } catch (e) {
      if (g !== listGen || isAbortError(e)) return
      listError.value = describe(s, e)
      agent.reportError(e)
    } finally {
      if (g === listGen) listLoading.value = false
    }
  })

  const refreshList = (): Promise<void> => listRefresh()

  // --- the open file

  const clearEditor = () => {
    editing.value = false
    draft.value = ''
    base.value = null
    eol.value = 'lf'
    saving.value = false
    saveError.value = null
    conflict.value = null
    serverChanged.value = false
  }

  const clearFile = () => {
    file.value = null
    fileLoading.value = false
    fileError.value = null
    fileMissing.value = false
    checkedEtag = null
    clearEditor()
  }

  // read fetches the open file; null when it is gone (404).
  const read = async (s: SessionHandle, name: string): Promise<MemoryFile | null> => {
    try {
      return fileOf(await s.client.getMemory(name, s.signal))
    } catch (e) {
      if (is(e, 404)) return null
      throw e
    }
  }

  const fileRefresh = coalesce(async () => {
    const s = session()
    const name = openName.value
    if (!s || name === null) return
    const g = fileGen
    const w = writeGen
    fileLoading.value = true
    try {
      const f = await read(s, name)
      if (g !== fileGen || w !== writeGen) return
      fileError.value = null
      if (f === null) {
        if (editing.value) {
          serverChanged.value = true
          return
        }
        file.value = null
        fileMissing.value = true
        return
      }
      fileMissing.value = false
      apply(s, f)
    } catch (e) {
      if (g !== fileGen || isAbortError(e)) return
      fileError.value = describe(s, e)
      agent.reportError(e)
    } finally {
      if (g === fileGen) fileLoading.value = false
    }
  })

  // apply takes a version read from the server: a clean editor follows
  // it, a changed draft is never touched (a banner says it changed).
  const apply = (s: SessionHandle, f: MemoryFile) => {
    const prev = file.value
    const known = editing.value ? base.value : prev
    const changed = known !== null && known.etag !== f.etag
    if (editing.value && dirty.value) {
      if (changed) serverChanged.value = true
      return
    }
    file.value = f
    if (editing.value) {
      if (!f.editable || f.truncated) {
        clearEditor()
      } else {
        setBase(f)
      }
    }
    if (changed) s.platform.notify('info', s.gettext('Updated by the agent'))
  }

  // setBase starts the draft from a version.
  const setBase = (f: MemoryFile) => {
    base.value = f
    eol.value = eolOf(f.content)
    draft.value = normalise(f.content)
  }

  // open shows the file with this name; answers for the one shown before
  // are dropped.
  const open = async (name: string): Promise<void> => {
    if (!session()) return
    fileGen++
    fileRefresh.clear()
    clearFile()
    openName.value = name
    if (!isMemoryName(name)) {
      fileMissing.value = true
      return
    }
    await fileRefresh()
  }

  const reload = (): Promise<void> => fileRefresh()

  const close = () => {
    fileGen++
    fileRefresh.clear()
    clearFile()
    openName.value = null
  }

  // --- editing

  const startEdit = () => {
    const f = file.value
    if (!f || !canEdit.value || editing.value) return
    setBase(f)
    saveError.value = null
    conflict.value = null
    serverChanged.value = false
    editing.value = true
  }

  // confirmLeave asks before a changed draft is left; true when it may go.
  const confirmLeave = async (): Promise<boolean> => {
    const s = session()
    if (!s || !dirty.value) return true
    return s.platform.confirm({
      title: s.gettext('Discard your changes?'),
      message: s.gettext('Your changes to this file have not been saved.'),
      confirmText: s.gettext('Discard changes'),
      danger: true
    })
  }

  const cancelEdit = async (): Promise<boolean> => {
    if (!editing.value) return true
    const g = fileGen
    if (!(await confirmLeave()) || g !== fileGen) return false
    clearEditor()
    return true
  }

  // saved ends the editor with the version just written.
  const saved = (s: SessionHandle, content: string, etag: string) => {
    const name = openName.value!
    const f: MemoryFile = { name, content, etag, truncated: false, editable: true }
    writeGen++
    file.value = f
    if (typedSince(content)) {
      // typed while the save ran: keep editing on top of the saved version
      base.value = f
      conflict.value = null
      serverChanged.value = false
    } else {
      clearEditor()
    }
    s.platform.notify('success', s.gettext('Saved'))
    if (listed) void listRefresh()
  }

  // typedSince reports a draft that changed after content was sent.
  const typedSince = (content: string) => editing.value && draft.value !== normalise(content)

  // savedUnknown handles a write whose answer has no etag: the version is
  // read again; a draft typed meanwhile stays (its next save conflicts).
  const savedUnknown = async (content: string) => {
    writeGen++
    if (!typedSince(content)) clearEditor()
    await fileRefresh()
  }

  // showConflict reads the current version after a 412 (or a 404: deleted)
  // and keeps the draft.
  const showConflict = async (s: SessionHandle, g: number) => {
    const name = openName.value!
    let current: MemoryFile | null
    try {
      current = await read(s, name)
    } catch (e) {
      if (g !== fileGen || isAbortError(e)) return
      saveError.value = s.gettext('The file changed since you opened it, and the current version could not be read.')
      return
    }
    if (g !== fileGen) return
    conflict.value = { current }
    serverChanged.value = false
  }

  // put writes the draft with the etag given; true when saved.
  const put = async (s: SessionHandle, etag: string): Promise<boolean> => {
    const name = openName.value
    if (name === null || saving.value) return false
    const content = toSave.value
    const problem = checkContent(content)
    if (problem) {
      saveError.value = contentMessage(s, problem)
      return false
    }
    const g = fileGen
    saving.value = true
    saveError.value = null
    try {
      const v = await s.client.putMemory(name, content, etag, s.signal)
      if (g !== fileGen) return false
      const newEtag = (v as { etag?: unknown } | null)?.etag
      if (typeof newEtag !== 'string' || newEtag === '') {
        // saved, but the new version is unknown: read it again
        await savedUnknown(content)
        return true
      }
      saved(s, content, newEtag)
      return true
    } catch (e) {
      if (g !== fileGen || isAbortError(e)) return false
      if (is(e, 412) || is(e, 404)) {
        await showConflict(s, g)
      } else {
        saveError.value = writeMessage(s, e)
      }
      return false
    } finally {
      if (g === fileGen) saving.value = false
    }
  }

  // save writes the draft over the version it started from.
  const save = async (): Promise<boolean> => {
    const s = session()
    if (!s || !editing.value || saving.value || base.value === null) return false
    return put(s, base.value.etag)
  }

  // overwriteMine writes the draft over the current version of a conflict
  // after the user confirmed it. A file that changed once more shows the
  // conflict again.
  const overwriteMine = async (): Promise<boolean> => {
    const s = session()
    const current = conflict.value?.current
    if (!s || !current || saving.value) return false
    const g = fileGen
    const ok = await s.platform.confirm({
      title: s.gettext('Overwrite the current version?'),
      message: s.gettext('The current version of the file is replaced with yours. The changes made meanwhile are lost.'),
      confirmText: s.gettext('Overwrite'),
      danger: true
    })
    if (!ok || g !== fileGen || conflict.value?.current !== current || saving.value) return false
    return put(s, current.etag)
  }

  // useCurrent drops the draft for the current version after a confirm.
  const useCurrent = async (): Promise<boolean> => {
    const s = session()
    const current = conflict.value?.current
    if (!s || !current) return false
    const g = fileGen
    const ok = await s.platform.confirm({
      title: s.gettext('Discard your version?'),
      message: s.gettext('Your changes are discarded and the current version is shown.'),
      confirmText: s.gettext('Discard my version'),
      danger: true
    })
    if (!ok || g !== fileGen || conflict.value?.current !== current) return false
    file.value = current
    clearEditor()
    return true
  }

  // keepEditing closes the conflict view and keeps the draft. The draft
  // stays based on the version it started from, so the next save conflicts
  // again and shows both versions: only "Overwrite with mine" (confirmed)
  // replaces the other version.
  const keepEditing = () => {
    if (conflict.value === null) return
    conflict.value = null
    serverChanged.value = true
  }

  // recreate creates a file deleted meanwhile again with the draft.
  const recreate = async (): Promise<boolean> => {
    const s = session()
    const name = openName.value
    if (!s || name === null || conflict.value?.current !== null || saving.value) return false
    const content = toSave.value
    const problem = checkContent(content)
    if (problem) {
      saveError.value = contentMessage(s, problem)
      return false
    }
    const g = fileGen
    saving.value = true
    saveError.value = null
    try {
      const v = await s.client.createMemory(name, content, s.signal)
      if (g !== fileGen) return false
      const etag = (v as { etag?: unknown } | null)?.etag
      if (typeof etag !== 'string' || etag === '') {
        await savedUnknown(content)
        return true
      }
      saved(s, content, etag)
      return true
    } catch (e) {
      if (g !== fileGen || isAbortError(e)) return false
      if (e instanceof ApiError && e.code === 'already_exists') {
        await showConflict(s, g)
      } else {
        saveError.value = writeMessage(s, e)
      }
      return false
    } finally {
      if (g === fileGen) saving.value = false
    }
  }

  // --- creating

  // create makes a new file; true when it exists now.
  const create = async (name: string, content: string): Promise<boolean> => {
    const s = session()
    if (!s || creating.value) return false
    createError.value = null
    if (!isMemoryName(name)) {
      createError.value = s.gettext(
        'Use 1 to 64 lower-case letters, digits and hyphens, starting with a letter or digit.'
      )
      return false
    }
    const problem = checkContent(content)
    if (problem) {
      createError.value = contentMessage(s, problem)
      return false
    }
    creating.value = true
    try {
      await s.client.createMemory(name, content, s.signal)
      if (bound !== s) return false
      await listRefresh()
      return true
    } catch (e) {
      if (bound !== s || isAbortError(e)) return false
      createError.value = writeMessage(s, e)
      // someone else created it: show it
      if (e instanceof ApiError && e.code === 'already_exists') await listRefresh()
      return false
    } finally {
      if (bound === s) creating.value = false
    }
  }

  const clearCreateError = () => {
    createError.value = null
  }

  // --- live updates

  // checkOpen reads the open file again when the list shows another etag
  // for it (the agent or another client changed it).
  const checkOpen = async () => {
    if (openName.value === null) return
    const info = files.value.find((f) => f.name === openName.value)
    const known = editing.value ? base.value : file.value
    if (known === null) {
      // shown as gone: read it again once it is listed
      if (info && fileMissing.value && info.etag !== checkedEtag) {
        checkedEtag = info.etag
        await fileRefresh()
      }
      return
    }
    if (!info) {
      // gone from the list: read it to be sure
      if (checkedEtag !== '') {
        checkedEtag = ''
        await fileRefresh()
      }
      return
    }
    if (info.etag === known.etag) {
      checkedEtag = info.etag
      return
    }
    if (info.etag === checkedEtag) return
    checkedEtag = info.etag
    await fileRefresh()
  }

  const onEvent = async (e: StoreEvent) => {
    if (e.type === 'reset') {
      reset()
      return
    }
    if (!session()) return
    if (e.type === 'changed' && !e.kinds.includes('activity')) return
    if (!listed) return
    await listRefresh()
    await checkOpen()
  }

  // attach subscribes to the agent store's events while the memory screen
  // is shown; it returns the function that detaches again.
  const attach = (): (() => void) => {
    session()
    return agent.subscribe(onEvent)
  }

  const reset = () => {
    listGen++
    listRefresh.clear()
    files.value = []
    listLoading.value = false
    listLoaded.value = false
    listError.value = null
    listed = false
    fileGen++
    fileRefresh.clear()
    clearFile()
    openName.value = null
    creating.value = false
    createError.value = null
    bound = agent.session()
  }

  return {
    files,
    coreFiles,
    notes,
    missingCore,
    listLoading,
    listLoaded,
    listError,
    openName,
    file,
    fileLoading,
    fileError,
    fileMissing,
    canEdit,
    editing,
    draft,
    dirty,
    mixedLineEndings,
    contentProblem,
    toSave,
    saving,
    saveError,
    conflict,
    serverChanged,
    creating,
    createError,
    attach,
    refreshList,
    open,
    reload,
    close,
    startEdit,
    cancelEdit,
    confirmLeave,
    save,
    overwriteMine,
    useCurrent,
    keepEditing,
    recreate,
    create,
    clearCreateError,
    reset
  }
})
