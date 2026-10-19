import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, json } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import { MAX_FILE, memoryHandler, memoryState, setFile, type MemoryState } from '../test/memory'
import { useMemoryStore } from './memory'

let detach: (() => void) | undefined
afterEach(() => {
  detach?.()
  detach = undefined
})

const setup = async (st: MemoryState = memoryState(), extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? memoryHandler(st)(c))
  const agent = await startStore(server)
  const mem = useMemoryStore()
  detach = mem.attach()
  return { st, server, mem, ...agent }
}

const puts = (server: ReturnType<typeof fakeServer>) => server.calls.filter((c) => c.method === 'PUT')
const names = (list: { name: string }[]) => list.map((f) => f.name)

describe('useMemoryStore', () => {
  describe('the list', () => {
    it('orders profile and projects first, then the notes', async () => {
      const st = memoryState({ 'notes/b.md': 'b', 'projects.md': 'p', 'notes/a.md': 'a', 'profile.md': 'x' })
      const { mem } = await setup(st)
      await mem.refreshList()
      expect(names(mem.files)).toEqual(['profile.md', 'projects.md', 'notes/a.md', 'notes/b.md'])
      expect(names(mem.coreFiles)).toEqual(['profile.md', 'projects.md'])
      expect(names(mem.notes)).toEqual(['notes/a.md', 'notes/b.md'])
      expect(mem.missingCore).toEqual([])
      expect(mem.listLoaded).toBe(true)
    })

    it('names the core files that do not exist yet', async () => {
      const { mem } = await setup(memoryState({ 'projects.md': 'p' }))
      await mem.refreshList()
      expect(mem.missingCore).toEqual(['profile.md'])
    })

    it('drops entries that are not memory files and keeps the error text fixed', async () => {
      const { mem } = await setup(memoryState(), (c) =>
        c.path === '/memory'
          ? json({ files: [{ name: '../x', size: 1, etag: '"1"', modified: '' }, { name: 'profile.md', size: 1, etag: '"2"', modified: '' }] })
          : undefined
      )
      await mem.refreshList()
      expect(names(mem.files)).toEqual(['profile.md'])
    })

    it('shows a fixed error and keeps the files it had', async () => {
      const st = memoryState({ 'profile.md': 'x' })
      const { mem } = await setup(st)
      await mem.refreshList()
      st.list = apiErr(502, 'upstream_unavailable')
      await mem.refreshList()
      expect(mem.listError).toBe('ownCloud could not be reached. Try again later.')
      expect(names(mem.files)).toEqual(['profile.md'])
    })

    it('refreshes on an activity change and on a refresh, not on other kinds', async () => {
      const st = memoryState({ 'profile.md': 'x' })
      const { mem, server, events } = await setup(st)
      await mem.refreshList()
      const reads = () => server.count('GET', '/memory')
      const before = reads()
      await events().onChanged(['jobs'])
      await flushPromises()
      expect(reads()).toBe(before)
      setFile(st, 'notes/new.md', 'n')
      await events().onChanged(['activity'])
      await flushPromises()
      expect(reads()).toBe(before + 1)
      expect(names(mem.files)).toEqual(['profile.md', 'notes/new.md'])
    })

    it('reads nothing on events before the screen asked for it', async () => {
      const { server, events } = await setup(memoryState({ 'profile.md': 'x' }))
      await events().onChanged(['activity'])
      await flushPromises()
      expect(server.count('GET', '/memory')).toBe(0)
    })
  })

  describe('a file', () => {
    it('opens a file with its content and etag', async () => {
      const st = memoryState({ 'notes/a.md': 'hello' })
      const { mem, server } = await setup(st)
      await mem.open('notes/a.md')
      expect(mem.file).toEqual({ name: 'notes/a.md', content: 'hello', etag: st.files['notes/a.md'].etag, truncated: false, editable: true })
      expect(server.calls.at(-1)!.path).toBe('/memory/notes/a.md')
    })

    it('refuses a name the server would refuse without asking it', async () => {
      const { mem, server } = await setup()
      const n = server.calls.length
      await mem.open('notes/../profile.md')
      expect(server.calls.length).toBe(n)
      expect(mem.fileMissing).toBe(true)
    })

    it('shows a truncated file read only', async () => {
      const { mem } = await setup(memoryState({ 'profile.md': 'a'.repeat(MAX_FILE + 10) }))
      await mem.open('profile.md')
      expect(mem.file!.truncated).toBe(true)
      expect(mem.canEdit).toBe(false)
      mem.startEdit()
      expect(mem.editing).toBe(false)
    })

    it('does not edit a file marked not editable', async () => {
      const { mem } = await setup(memoryState(), (c) =>
        c.path === '/memory/profile.md'
          ? json({ name: 'profile.md', content: 'x', etag: '"1"', truncated: false, editable: false })
          : undefined
      )
      await mem.open('profile.md')
      expect(mem.canEdit).toBe(false)
    })

    it('says when a file is too large to open, not text or gone', async () => {
      const st = memoryState()
      st.get = { 'profile.md': apiErr(422, 'file_too_large'), 'projects.md': apiErr(422, 'not_text') }
      const { mem } = await setup(st)
      await mem.open('profile.md')
      expect(mem.fileError).toBe('The file is too large to open.')
      await mem.open('projects.md')
      expect(mem.fileError).toBe('The file is not plain text.')
      await mem.open('notes/gone.md')
      expect(mem.fileMissing).toBe(true)
      expect(mem.fileError).toBeNull()
    })
  })

  describe('editing', () => {
    const editing = async (content = 'old', extra?: Handler) => {
      const st = memoryState({ 'notes/a.md': content })
      const s = await setup(st, extra)
      await s.mem.open('notes/a.md')
      s.mem.startEdit()
      return s
    }

    it('starts with the content and is clean until changed', async () => {
      const { mem } = await editing()
      expect(mem.editing).toBe(true)
      expect(mem.draft).toBe('old')
      expect(mem.dirty).toBe(false)
      mem.draft = 'new'
      expect(mem.dirty).toBe(true)
    })

    it('saves with the etag exactly as returned, quotes included', async () => {
      const { mem, server, st, platform } = await editing()
      const etag = st.files['notes/a.md'].etag
      expect(etag.startsWith('"')).toBe(true)
      mem.draft = 'new'
      expect(await mem.save()).toBe(true)
      const put = puts(server)[0]
      expect(put.path).toBe('/memory/notes/a.md')
      expect(put.headers.get('If-Match')).toBe(etag)
      expect(put.body).toEqual({ content: 'new' })
      expect(mem.editing).toBe(false)
      expect(mem.file!.content).toBe('new')
      expect(mem.file!.etag).toBe(st.files['notes/a.md'].etag)
      expect(platform.notify).toHaveBeenCalledWith('success', 'Saved')
    })

    it('keeps what was typed while the save ran', async () => {
      const { mem, st } = await editing()
      mem.draft = 'new'
      const p = mem.save()
      mem.draft = 'new and more'
      expect(await p).toBe(true)
      expect(st.files['notes/a.md'].content).toBe('new')
      expect(mem.editing).toBe(true)
      expect(mem.draft).toBe('new and more')
      expect(mem.dirty).toBe(true)
      expect(mem.file!.content).toBe('new')
      // the next save builds on the version just written
      expect(await mem.save()).toBe(true)
      expect(st.files['notes/a.md'].content).toBe('new and more')
    })

    it('keeps what was typed while a save without an etag in its answer ran', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem } = await setup(st)
      await mem.open('notes/a.md')
      mem.startEdit()
      st.put = json({ name: 'notes/a.md' })
      mem.draft = 'new'
      const p = mem.save()
      mem.draft = 'new and more'
      expect(await p).toBe(true)
      expect(mem.editing).toBe(true)
      expect(mem.draft).toBe('new and more')
    })

    it('drops a read that started before its own save', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      let release: (() => void) | undefined
      let hold = false
      const { mem, platform } = await setup(st, (c) => {
        if (hold && c.method === 'GET' && c.path === '/memory/notes/a.md') {
          const r = memoryHandler(st)(c) as Response
          return new Promise<Response>((res) => {
            release = () => res(r)
          })
        }
        return undefined
      })
      await mem.open('notes/a.md')
      mem.startEdit()
      hold = true
      const reading = mem.reload()
      await flushPromises()
      hold = false
      mem.draft = 'new'
      expect(await mem.save()).toBe(true)
      release!()
      await reading
      await flushPromises()
      expect(mem.file!.content).toBe('new')
      expect(platform.notify).not.toHaveBeenCalledWith('info', 'Updated by the agent')
    })

    describe('line endings', () => {
      it('keeps CRLF line endings of a file and does not count them as a change', async () => {
        const { mem, st, server } = await editing('a\r\nb\r\n')
        expect(mem.draft).toBe('a\nb\n')
        expect(mem.dirty).toBe(false)
        expect(mem.mixedLineEndings).toBe(false)
        mem.draft = 'a\nb\nc\n'
        expect(mem.dirty).toBe(true)
        expect(await mem.save()).toBe(true)
        expect(puts(server)[0].body).toEqual({ content: 'a\r\nb\r\nc\r\n' })
        expect(st.files['notes/a.md'].content).toBe('a\r\nb\r\nc\r\n')
        expect(mem.file!.content).toBe('a\r\nb\r\nc\r\n')
      })

      it('warns that mixed line endings are saved as LF', async () => {
        const { mem, server } = await editing('a\r\nb\nc\rd')
        expect(mem.draft).toBe('a\nb\nc\nd')
        expect(mem.dirty).toBe(false)
        expect(mem.mixedLineEndings).toBe(true)
        mem.draft = 'a\nb\nc\nd!'
        await mem.save()
        expect(puts(server)[0].body).toEqual({ content: 'a\nb\nc\nd!' })
      })

      it('counts the size with the line endings that are saved', async () => {
        const { mem } = await editing('a\r\n')
        mem.draft = '\n'.repeat(MAX_FILE / 2 + 1)
        expect(mem.contentProblem).toBe('too_large')
      })
    })

    it('sends no second save while one runs', async () => {
      const { mem, server } = await editing()
      mem.draft = 'new'
      const a = mem.save()
      const b = mem.save()
      await Promise.all([a, b])
      expect(puts(server)).toHaveLength(1)
    })

    it('refuses NUL and lone surrogates before sending and keeps the draft', async () => {
      const { mem, server } = await editing()
      mem.draft = 'a\u0000b'
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('The text contains a NUL character, which a memory file cannot hold.')
      mem.draft = 'a\ud800'
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('The text contains an invalid character (a lone surrogate). Remove it and save again.')
      expect(puts(server)).toHaveLength(0)
      expect(mem.draft).toBe('a\ud800')
      expect(mem.editing).toBe(true)
    })

    it('refuses a draft above the size limit before sending', async () => {
      const { mem, server } = await editing()
      mem.draft = 'a'.repeat(MAX_FILE + 1)
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('A memory file can hold at most 64 KiB. Shorten the text and save again.')
      expect(puts(server)).toHaveLength(0)
    })

    it('shows the size message on a 413 and keeps the draft', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem } = await setup(st)
      await mem.open('notes/a.md')
      mem.startEdit()
      st.put = apiErr(413, 'too_large')
      mem.draft = 'new'
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('A memory file can hold at most 64 KiB. Shorten the text and save again.')
      expect(mem.draft).toBe('new')
      expect(mem.editing).toBe(true)
    })

    it('shows the generic invalid message on a 400', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem } = await setup(st)
      await mem.open('notes/a.md')
      mem.startEdit()
      st.put = apiErr(400, 'invalid_request')
      mem.draft = 'new'
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('The request was not accepted. Check your input and try again.')
      expect(mem.draft).toBe('new')
    })

    it('on a 428 keeps the draft and says the version is missing', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem } = await setup(st)
      await mem.open('notes/a.md')
      mem.startEdit()
      st.put = apiErr(428, 'precondition_required')
      mem.draft = 'new'
      expect(await mem.save()).toBe(false)
      expect(mem.saveError).toBe('The file version is missing. Reload the file and try again.')
      expect(mem.draft).toBe('new')
      expect(mem.editing).toBe(true)
    })

    describe('a conflict (412)', () => {
      const conflicted = async () => {
        const s = await editing('old')
        setFile(s.st, 'notes/a.md', 'theirs')
        s.mem.draft = 'mine'
        expect(await s.mem.save()).toBe(false)
        return s
      }

      it('reads the current version and keeps the draft', async () => {
        const { mem, st } = await conflicted()
        expect(mem.conflict).toEqual({
          current: { name: 'notes/a.md', content: 'theirs', etag: st.files['notes/a.md'].etag, truncated: false, editable: true }
        })
        expect(mem.draft).toBe('mine')
        expect(mem.editing).toBe(true)
        expect(mem.dirty).toBe(true)
        expect(mem.saveError).toBeNull()
      })

      it('overwrites with mine after a confirm, with the new etag', async () => {
        const { mem, st, server, platform } = await conflicted()
        const newer = st.files['notes/a.md'].etag
        expect(await mem.overwriteMine()).toBe(true)
        expect(platform.confirm).toHaveBeenCalledTimes(1)
        expect(puts(server).at(-1)!.headers.get('If-Match')).toBe(newer)
        expect(st.files['notes/a.md'].content).toBe('mine')
        expect(mem.conflict).toBeNull()
        expect(mem.editing).toBe(false)
      })

      it('does nothing when the overwrite is not confirmed', async () => {
        const { mem, st, server, platform } = await conflicted()
        platform.confirm.mockResolvedValueOnce(false)
        const n = puts(server).length
        expect(await mem.overwriteMine()).toBe(false)
        expect(puts(server)).toHaveLength(n)
        expect(mem.conflict).not.toBeNull()
        expect(st.files['notes/a.md'].content).toBe('theirs')
      })

      it('shows the conflict again when it changed once more before the overwrite', async () => {
        const { mem, st } = await conflicted()
        setFile(st, 'notes/a.md', 'theirs again')
        expect(await mem.overwriteMine()).toBe(false)
        expect(mem.conflict!.current!.content).toBe('theirs again')
        expect(mem.draft).toBe('mine')
        expect(st.files['notes/a.md'].content).toBe('theirs again')
      })

      it('uses the current version after a confirm and drops the draft', async () => {
        const { mem, platform } = await conflicted()
        expect(await mem.useCurrent()).toBe(true)
        expect(platform.confirm).toHaveBeenCalledTimes(1)
        expect(mem.editing).toBe(false)
        expect(mem.conflict).toBeNull()
        expect(mem.file!.content).toBe('theirs')
      })

      it('keeps the draft when using the current version is not confirmed', async () => {
        const { mem, platform } = await conflicted()
        platform.confirm.mockResolvedValueOnce(false)
        expect(await mem.useCurrent()).toBe(false)
        expect(mem.draft).toBe('mine')
        expect(mem.conflict).not.toBeNull()
      })

      it('keeps editing without taking over the other version, so the next save conflicts again', async () => {
        const { mem, st, server } = await conflicted()
        const original = puts(server)[0].headers.get('If-Match')
        mem.keepEditing()
        expect(mem.conflict).toBeNull()
        expect(mem.draft).toBe('mine')
        expect(mem.editing).toBe(true)
        expect(mem.serverChanged).toBe(true)
        mem.draft = 'merged'
        expect(await mem.save()).toBe(false)
        expect(puts(server).at(-1)!.headers.get('If-Match')).toBe(original)
        expect(st.files['notes/a.md'].content).toBe('theirs')
        expect(mem.conflict!.current!.content).toBe('theirs')
        expect(mem.draft).toBe('merged')
        expect(mem.serverChanged).toBe(false)
      })

      it('says so when the current version cannot be read after a conflict', async () => {
        const s = await editing('old')
        setFile(s.st, 'notes/a.md', 'theirs')
        s.st.get = { 'notes/a.md': apiErr(502, 'upstream_unavailable') }
        s.mem.draft = 'mine'
        expect(await s.mem.save()).toBe(false)
        expect(s.mem.saveError).toBe(
          'The file changed since you opened it, and the current version could not be read.'
        )
        expect(s.mem.conflict).toBeNull()
        expect(s.mem.draft).toBe('mine')
        expect(s.mem.editing).toBe(true)
      })

      it('runs one overwrite at a time, also after two confirms', async () => {
        const { mem, server } = await conflicted()
        const n = puts(server).length
        const a = mem.overwriteMine()
        const b = mem.overwriteMine()
        await Promise.all([a, b])
        expect(puts(server)).toHaveLength(n + 1)
      })

      it('shows the conflict when a deleted file was created again meanwhile', async () => {
        const s = await editing('old')
        delete s.st.files['notes/a.md']
        s.mem.draft = 'mine'
        s.st.put = apiErr(404, 'not_found')
        await s.mem.save()
        s.st.put = undefined
        setFile(s.st, 'notes/a.md', 'someone else')
        expect(await s.mem.recreate()).toBe(false)
        expect(s.mem.conflict!.current!.content).toBe('someone else')
        expect(s.mem.draft).toBe('mine')
        expect(s.st.files['notes/a.md'].content).toBe('someone else')
      })

      it('offers to create the file again when it was deleted meanwhile', async () => {
        const s = await editing('old')
        delete s.st.files['notes/a.md']
        s.mem.draft = 'mine'
        s.st.put = apiErr(404, 'not_found')
        expect(await s.mem.save()).toBe(false)
        s.st.put = undefined
        expect(s.mem.conflict).toEqual({ current: null })
        expect(s.mem.draft).toBe('mine')
        expect(await s.mem.recreate()).toBe(true)
        expect(s.st.files['notes/a.md'].content).toBe('mine')
        expect(s.mem.editing).toBe(false)
        expect(s.mem.file!.content).toBe('mine')
      })
    })

    it('asks before discarding a changed draft', async () => {
      const { mem, platform } = await editing()
      expect(await mem.cancelEdit()).toBe(true)
      expect(platform.confirm).not.toHaveBeenCalled()
      mem.startEdit()
      mem.draft = 'changed'
      platform.confirm.mockResolvedValueOnce(false)
      expect(await mem.cancelEdit()).toBe(false)
      expect(mem.draft).toBe('changed')
      expect(await mem.cancelEdit()).toBe(true)
      expect(mem.editing).toBe(false)
    })

    it('confirmLeave asks only while the draft is changed', async () => {
      const { mem, platform } = await editing()
      expect(await mem.confirmLeave()).toBe(true)
      mem.draft = 'changed'
      platform.confirm.mockResolvedValueOnce(false)
      expect(await mem.confirmLeave()).toBe(false)
      expect(await mem.confirmLeave()).toBe(true)
      expect(platform.confirm).toHaveBeenCalledTimes(2)
    })
  })

  describe('live changes', () => {
    it('reloads an open file whose etag changed, with a note', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events, platform } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      setFile(st, 'notes/a.md', 'by the agent')
      await events().onChanged(['activity'])
      await flushPromises()
      expect(mem.file!.content).toBe('by the agent')
      expect(platform.notify).toHaveBeenCalledWith('info', 'Updated by the agent')
    })

    it('does not re-read the file when its etag did not change', async () => {
      const st = memoryState({ 'notes/a.md': 'old', 'profile.md': 'p' })
      const { mem, events, server, platform } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      setFile(st, 'profile.md', 'p2')
      await events().onChanged(['activity'])
      await flushPromises()
      expect(server.count('GET', '/memory/notes/a.md')).toBe(1)
      expect(platform.notify).not.toHaveBeenCalled()
    })

    it('does not note its own save as a change', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events, platform } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      mem.startEdit()
      mem.draft = 'mine'
      await mem.save()
      platform.notify.mockClear()
      await events().onChanged(['activity'])
      await flushPromises()
      expect(platform.notify).not.toHaveBeenCalled()
      expect(mem.file!.content).toBe('mine')
    })

    it('updates a clean editor with the new content', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events, server } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      mem.startEdit()
      setFile(st, 'notes/a.md', 'by the agent')
      await events().onChanged(['activity'])
      await flushPromises()
      expect(mem.draft).toBe('by the agent')
      expect(mem.dirty).toBe(false)
      const agentEtag = st.files['notes/a.md'].etag
      mem.draft = 'mine'
      await mem.save()
      expect(puts(server).at(-1)!.headers.get('If-Match')).toBe(agentEtag)
    })

    it('never overwrites a changed draft and shows that the server changed', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events, platform } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      mem.startEdit()
      mem.draft = 'mine'
      setFile(st, 'notes/a.md', 'by the agent')
      await events().onChanged(['activity'])
      await flushPromises()
      expect(mem.draft).toBe('mine')
      expect(mem.serverChanged).toBe(true)
      expect(platform.notify).not.toHaveBeenCalled()
      // the save then goes through the conflict view
      expect(await mem.save()).toBe(false)
      expect(mem.conflict!.current!.content).toBe('by the agent')
    })

    it('shows a file deleted on the server as gone when not editing', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      delete st.files['notes/a.md']
      await events().onChanged(['activity'])
      await flushPromises()
      expect(mem.fileMissing).toBe(true)
    })

    it('keeps a changed draft when the file was deleted on the server', async () => {
      const st = memoryState({ 'notes/a.md': 'old' })
      const { mem, events } = await setup(st)
      await mem.refreshList()
      await mem.open('notes/a.md')
      mem.startEdit()
      mem.draft = 'mine'
      delete st.files['notes/a.md']
      await events().onChanged(['activity'])
      await flushPromises()
      expect(mem.draft).toBe('mine')
      expect(mem.editing).toBe(true)
      expect(mem.serverChanged).toBe(true)
    })
  })

  describe('creating', () => {
    it('creates a note and refreshes the list', async () => {
      const st = memoryState()
      const { mem, server } = await setup(st)
      await mem.refreshList()
      expect(await mem.create('notes/trip.md', 'plan')).toBe(true)
      expect(server.calls.find((c) => c.method === 'POST')!.body).toEqual({ name: 'notes/trip.md', content: 'plan' })
      expect(st.files['notes/trip.md'].content).toBe('plan')
      expect(names(mem.files)).toEqual(['notes/trip.md'])
    })

    it('refuses an invalid name without asking the server', async () => {
      const { mem, server } = await setup()
      expect(await mem.create('notes/Trip.md', '')).toBe(false)
      expect(mem.createError).toBe('Use 1 to 64 lower-case letters, digits and hyphens, starting with a letter or digit.')
      expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(0)
    })

    it('refreshes the list when the name exists already', async () => {
      const st = memoryState()
      const { mem } = await setup(st)
      await mem.refreshList()
      expect(mem.missingCore).toEqual(['profile.md', 'projects.md'])
      setFile(st, 'profile.md', 'by the agent')
      expect(await mem.create('profile.md', '')).toBe(false)
      await flushPromises()
      expect(mem.missingCore).toEqual(['projects.md'])
      expect(st.files['profile.md'].content).toBe('by the agent')
    })

    it('says when the name exists already', async () => {
      const { mem } = await setup(memoryState({ 'notes/trip.md': 'x' }))
      expect(await mem.create('notes/trip.md', '')).toBe(false)
      expect(mem.createError).toBe('A file with this name already exists.')
    })

    it('refuses invalid content before sending', async () => {
      const { mem, server } = await setup()
      expect(await mem.create('notes/a.md', 'x\u0000')).toBe(false)
      expect(mem.createError).toBe('The text contains a NUL character, which a memory file cannot hold.')
      expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(0)
    })
  })

  it('forgets everything when the session ends', async () => {
    const st = memoryState({ 'notes/a.md': 'old' })
    const { mem, store } = await setup(st)
    await mem.refreshList()
    await mem.open('notes/a.md')
    mem.startEdit()
    mem.draft = 'mine'
    store.stop()
    await flushPromises()
    expect(mem.files).toEqual([])
    expect(mem.file).toBeNull()
    expect(mem.editing).toBe(false)
    expect(mem.draft).toBe('')
  })
})
