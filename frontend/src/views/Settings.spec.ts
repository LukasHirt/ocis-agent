import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Settings from './Settings.vue'
import { PLATFORM_KEY } from '../platform'
import { apiErr, json, settingsOf, statusOf } from '../test/fixtures'
import { fakeServer, startStore, type Call, type Handler } from '../test/server'
import type { ProactiveState, Settings as SettingsT, Status } from '../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const models = {
  default: 'm',
  available: [
    { id: 'm', label: 'Model A' },
    { id: 'm2', label: 'Model B' }
  ],
  preferences: { chat: null, proactive: null }
}

// a fake settings resource: PUT applies the body like the server does
const settingsServer = (initial: Partial<SettingsT> = {}, status: Partial<Status> = {}, extra?: Handler) => {
  let cur = settingsOf({ watch: ['/a'], daily_tokens: 50000, daily_tokens_max: 100000, ...initial })
  const put: unknown[] = []
  const handler: Handler = (c: Call) => {
    const r = extra?.(c)
    if (r) return r
    if (c.method === 'GET' && c.path === '/status') return json(statusOf({ models, ...status }))
    if (c.method === 'GET' && c.path === '/settings') return json(cur)
    if (c.method === 'PUT' && c.path === '/settings') {
      put.push(c.body)
      const b = c.body as Record<string, unknown>
      const next = { ...cur, models: { ...cur.models } } as SettingsT
      if ('proactive_enabled' in b) next.proactive_enabled = b.proactive_enabled as boolean
      if ('watch' in b) next.watch = b.watch as string[]
      if ('sweep_interval' in b) next.sweep_interval = ({ '5m': '5m0s', '1h': '1h0m0s', '168h': '168h0m0s' } as Record<string, string>)[b.sweep_interval as string] ?? String(b.sweep_interval)
      if ('quiet_hours' in b) next.quiet_hours = b.quiet_hours as string
      if ('daily_tokens' in b) next.daily_tokens = b.daily_tokens === null ? cur.daily_tokens_max : (b.daily_tokens as number)
      if ('models' in b) Object.assign(next.models, b.models)
      cur = next
      return json(cur)
    }
    return undefined
  }
  return { handler, put, set: (s: Partial<SettingsT>) => (cur = { ...cur, ...s }) }
}

const setup = async (handler: Handler) => {
  const server = fakeServer(handler)
  const { store, platform } = await startStore(server)
  wrapper = mount(Settings, { global: { provide: { [PLATFORM_KEY as symbol]: platform } } })
  await flushPromises()
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)
const save = async (w: VueWrapper) => {
  await button(w, 'Save')!.trigger('click')
  await flushPromises()
}
const input = (w: VueWrapper, id: string) => w.find<HTMLInputElement>(`input[data-testid="${id}"]`)
const select = (w: VueWrapper, id: string) => w.find<HTMLSelectElement>(`select[data-testid="${id}"]`)
// fieldError is the error message the text input shows (and links to the
// input), or null when it is valid.
const fieldError = (w: VueWrapper, id: string): string | null => {
  const el = input(w, id)
  if (el.attributes('aria-invalid') !== 'true') return null
  const msg = el.attributes('aria-describedby')
  return msg ? w.find(`[id="${msg}"]`).text() : ''
}
const optionTexts = (w: VueWrapper, id: string) => select(w, id).findAll('option').map((o) => o.text())
const choose = async (w: VueWrapper, id: string, text: string) => {
  const i = optionTexts(w, id).indexOf(text)
  expect(i, `option ${text}`).toBeGreaterThanOrEqual(0)
  await select(w, id).setValue(String(i))
}

describe('Settings', () => {
  it('shows a spinner until the settings are loaded', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const s = settingsServer()
    const { w } = await setup((c) => (c.path === '/settings' ? gate.then(() => s.handler(c)!) : s.handler(c)))
    expect(w.find('[data-stub="OcSpinner"]').exists()).toBe(true)
    expect(button(w, 'Save')).toBeUndefined()
    release()
    await flushPromises()
    expect(button(w, 'Save')).toBeDefined()
  })

  it('shows a fixed error with a retry when the settings cannot be loaded', async () => {
    let fail = true
    const s = settingsServer()
    const { w } = await setup((c) => (c.path === '/settings' && fail ? apiErr(503, 'unavailable') : s.handler(c)))
    expect(w.find('[role="alert"]').text()).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await button(w, 'Try again')!.trigger('click')
    await flushPromises()
    expect(button(w, 'Save')).toBeDefined()
  })

  it('save and discard are disabled while nothing changed', async () => {
    const { w } = await setup(settingsServer().handler)
    expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
    expect(button(w, 'Discard changes')!.attributes('disabled')).toBeDefined()
  })

  it('never offers or sends auto_approve_write, paused or dormancy_days', async () => {
    const s = settingsServer({ ...({ auto_approve_write: true } as object) })
    const { w } = await setup(s.handler)
    expect(w.html()).not.toContain('auto_approve')
    expect(w.text().toLowerCase()).not.toContain('auto-approve')
    await w.find('[data-testid="agent-proactive"] [role="switch"]').trigger('click')
    await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
    await save(w)
    expect(s.put).toHaveLength(1)
    const keys = Object.keys(s.put[0] as object)
    for (const k of ['auto_approve_write', 'paused', 'dormancy_days', 'daily_tokens_max']) expect(keys).not.toContain(k)
  })

  describe('budget', () => {
    it("shows today's usage and when it resets", async () => {
      const { w } = await setup(
        settingsServer({}, {
          budget: { used_tokens: 25000, daily_tokens: 50000, resets_at: '2026-01-02T00:00:00Z' }
        }).handler
      )
      const p = w.find('progress')
      expect(p.attributes('value')).toBe('25000')
      expect(p.attributes('max')).toBe('50000')
      expect(w.find('[data-testid="agent-budget-usage"]').text()).toMatch(/25.?000/)
      expect(w.find('[data-testid="agent-budget-usage"]').text()).toMatch(/50.?000/)
      expect(w.find('[data-testid="agent-budget-reset"]').text()).toContain('00:00 UTC')
    })

    it.each([
      ['999', false],
      ['1000', true],
      ['100000', true],
      ['100001', false],
      ['1500.5', false],
      ['abc', false],
      ['', false]
    ])('validates %j', async (v, ok) => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-daily-tokens').setValue(v)
      expect(fieldError(w, 'agent-daily-tokens') !== null).toBe(!ok)
      expect(button(w, 'Save')!.attributes('disabled') === undefined).toBe(ok)
      if (ok) {
        await save(w)
        expect(JSON.stringify(s.put)).toBe(`[{"daily_tokens":${v}}]`)
      } else {
        await save(w)
        expect(s.put).toHaveLength(0)
      }
    })

    it('names the allowed range', async () => {
      const { w } = await setup(settingsServer({ daily_tokens_max: 70000 }).handler)
      await input(w, 'agent-daily-tokens').setValue('70001')
      expect(fieldError(w, 'agent-daily-tokens')).toMatch(/1.?000.*70.?000/)
    })

    it("sends null to follow the operator's limit", async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await button(w, "Follow the operator's limit")!.trigger('click')
      expect(input(w, 'agent-daily-tokens').element.value).toBe('100000')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"daily_tokens":null}]')
      expect(input(w, 'agent-daily-tokens').element.value).toBe('100000')
    })

    it('typing after following the operator sends the number again', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await button(w, "Follow the operator's limit")!.trigger('click')
      await input(w, 'agent-daily-tokens').setValue('3000')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"daily_tokens":3000}]')
    })
  })

  describe('models', () => {
    it('offers the operator default and the catalog for both task kinds', async () => {
      const { w } = await setup(settingsServer().handler)
      for (const id of ['agent-model-chat', 'agent-model-proactive']) {
        expect(optionTexts(w, id)).toEqual(['Operator default (Model A)', 'Model A (m)', 'Model B (m2)'])
      }
      expect(w.find('[data-testid="agent-model-chat-effective"]').text()).toContain('Model A')
    })

    it('sends only the changed task', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await choose(w, 'agent-model-chat', 'Model B (m2)')
      expect(w.find('[data-testid="agent-model-chat-effective"]').text()).toContain('Model B')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"models":{"chat":"m2"}}]')
    })

    it('sends null to clear a preference', async () => {
      const s = settingsServer({ models: { chat: null, proactive: 'm2' } })
      const { w } = await setup(s.handler)
      expect(select(w, 'agent-model-proactive').element.value).toBe('2')
      await choose(w, 'agent-model-proactive', 'Operator default (Model A)')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"models":{"proactive":null}}]')
    })

    it('says so when the operator offers no models', async () => {
      const { w } = await setup(
        settingsServer({}, { models: { default: '', available: [], preferences: { chat: null, proactive: null } } }).handler
      )
      expect(w.text()).toContain('The operator offers no models to choose from')
      expect(select(w, 'agent-model-chat').exists()).toBe(false)
    })
  })

  describe('policy', () => {
    it('toggles proactive work', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await w.find('[data-testid="agent-proactive"] [role="switch"]').trigger('click')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"proactive_enabled":true}]')
    })

    it('adds a cleaned folder to the watch list', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-watch-new').setValue('//Docs/')
      await button(w, 'Add')!.trigger('click')
      expect(w.findAll('[data-testid="agent-watch-item"]').map((e) => e.find('pre, code').text())).toEqual(['/a', '/Docs'])
      expect(input(w, 'agent-watch-new').element.value).toBe('')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"watch":["/a","/Docs"]}]')
    })

    it('adds on Enter in the field only', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-watch-new').setValue('/B')
      // a keydown that only reaches the field's wrapper adds nothing
      await w.find('[data-stub="OcTextInput"][data-testid="agent-watch-new"]').trigger('keydown', { key: 'Enter' })
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(1)
      await input(w, 'agent-watch-new').trigger('keydown', { key: 'Enter' })
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(2)
      expect(fieldError(w, 'agent-watch-new')).toBeNull()
    })

    it.each([
      ['Docs', 'must start with /'],
      ['/a/../b', '. or ..'],
      ['/./b', '. or ..'],
      ['/.ocis-agent', "agent's own folder"],
      ['/.ocis-agent/memory', "agent's own folder"],
      ['/a\u202eb', 'control characters'],
      ['', 'Enter a folder'],
      ['/a', 'already in the list'],
      ['/' + 'x'.repeat(1024), 'too long']
    ])('refuses %j', async (p, msg) => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-watch-new').setValue(p)
      await button(w, 'Add')!.trigger('click')
      expect(fieldError(w, 'agent-watch-new')).toContain(msg)
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(1)
      expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
    })

    it('allows at most 20 folders', async () => {
      const s = settingsServer({ watch: Array.from({ length: 19 }, (_, i) => `/f${i}`) })
      const { w } = await setup(s.handler)
      await input(w, 'agent-watch-new').setValue('/twenty')
      await button(w, 'Add')!.trigger('click')
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(20)
      expect(button(w, 'Add')!.attributes('disabled')).toBeDefined()
      expect(w.text()).toContain('At most 20 folders can be watched')
      await input(w, 'agent-watch-new').setValue('/more')
      await input(w, 'agent-watch-new').trigger('keydown', { key: 'Enter' })
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(20)
    })

    it('removes a folder', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await w.find('[data-testid="agent-watch-item"] button').trigger('click')
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(0)
      expect(w.text()).toContain('No folders are watched')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"watch":[]}]')
    })

    it('shows hostile watched paths as text', async () => {
      const { w } = await setup(settingsServer({ watch: ['/<img src=x onerror=alert(1)>/\u202ex'] }).handler)
      const item = w.find('[data-testid="agent-watch-item"]')
      expect(item.text()).toContain('<img src=x onerror=alert(1)>')
      expect(item.text()).toContain('\\u{202E}')
      expect(w.find('img').exists()).toBe(false)
      expect(item.find('button').attributes('aria-label')).toContain('\\u{202E}')
    })

    it('offers the sweep presets and sends the chosen one', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      expect(optionTexts(w, 'agent-sweep')).toEqual([
        'Every 5 minutes',
        'Every 15 minutes',
        'Every hour',
        'Every 6 hours',
        'Every day',
        'Every 7 days'
      ])
      expect(select(w, 'agent-sweep').element.value).toBe('1')
      await choose(w, 'agent-sweep', 'Every 7 days')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"sweep_interval":"168h"}]')
      expect(select(w, 'agent-sweep').element.value).toBe('5')
    })

    it('keeps a stored sweep interval that is not a preset', async () => {
      const s = settingsServer({ sweep_interval: '2h0m0s' })
      const { w } = await setup(s.handler)
      expect(optionTexts(w, 'agent-sweep')[0]).toBe('Every 2h (current)')
      expect(select(w, 'agent-sweep').element.value).toBe('0')
      await choose(w, 'agent-sweep', 'Every 5 minutes')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"sweep_interval":"5m"}]')
    })

    it.each([
      ['22:00-07:00', true],
      ['00:00-23:59', true],
      ['24:00-07:00', false],
      ['22:00-22:00', false],
      ['22:00 07:00', false],
      ['7:00-9:00', false]
    ])('validates quiet hours %j', async (q, ok) => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-quiet-hours').setValue(q)
      expect(fieldError(w, 'agent-quiet-hours') !== null).toBe(!ok)
      await save(w)
      expect(JSON.stringify(s.put)).toBe(ok ? `[{"quiet_hours":"${q}"}]` : '[]')
    })

    const proactive = (over: Partial<ProactiveState> = {}): ProactiveState => ({
      enabled: true,
      last_sweep_at: '2026-01-01T06:00:00Z',
      next_sweep_at: '2026-01-01T12:00:00Z',
      blocked: '',
      daily_jobs: 24,
      ...over
    })

    it('explains in plain text what proactive work does, with the daily limit', async () => {
      const { w } = await setup(settingsServer({}, { proactive: proactive() }).handler)
      expect(w.find('[data-testid="agent-proactive-about"]').text()).toBe(
        'When proactive work is on, the agent reviews your recent changes and open tasks on its own: on the ' +
          'schedule below and soon after a watched folder changes. A review only reads and proposes: every ' +
          'action other than reading files or updating its memory waits for your approval in the approvals ' +
          'inbox. Quiet hours are in UTC; no review starts on its own during them. At most 24 reviews run ' +
          "per day (UTC), and none starts once 80% of the day's token budget is used."
      )
    })

    it('names the daily limit generally when the server does not say it', async () => {
      const { w } = await setup(settingsServer({}, { proactive: undefined }).handler)
      expect(w.find('[data-testid="agent-proactive-about"]').text()).toContain(
        "A daily limit applies to reviews, and none starts once 80% of the day's token budget is used."
      )
      expect(w.find('[data-testid="agent-proactive-last"]').exists()).toBe(false)
      expect(w.find('[data-testid="agent-proactive-next"]').exists()).toBe(false)
      expect(w.find('[data-testid="agent-proactive-blocked"]').exists()).toBe(false)
    })

    it('labels the quiet hours as UTC', async () => {
      const { w } = await setup(settingsServer().handler)
      expect(w.find('[data-testid="agent-quiet-hours"] label').text()).toBe(
        'Quiet hours in UTC (HH:MM-HH:MM, empty for none)'
      )
    })

    it('shows the last review and when the next is expected', async () => {
      const { w } = await setup(settingsServer({ proactive_enabled: true }, { proactive: proactive() }).handler)
      const last = w.find('[data-testid="agent-proactive-last"]')
      expect(last.text()).toContain('Last review')
      expect(last.find('time').attributes('datetime')).toBe('2026-01-01T06:00:00Z')
      const next = w.find('[data-testid="agent-proactive-next"]')
      expect(next.text()).toContain('Next scheduled review expected around')
      expect(next.find('time').attributes('datetime')).toBe('2026-01-01T12:00:00Z')
      expect(w.find('[data-testid="agent-proactive-blocked"]').exists()).toBe(false)
    })

    it('says when there was no review yet and shows no next one while off', async () => {
      const { w } = await setup(
        settingsServer({}, { proactive: proactive({ enabled: false, last_sweep_at: null, next_sweep_at: null, blocked: 'proactive_disabled' }) })
          .handler
      )
      expect(w.find('[data-testid="agent-proactive-last"]').text()).toBe('Last review: none yet')
      expect(w.find('[data-testid="agent-proactive-next"]').exists()).toBe(false)
      expect(w.find('[data-testid="agent-proactive-blocked"]').exists()).toBe(false)
    })

    it.each([
      ['kill_switch', 'The operator has stopped the agent, so no review starts now.'],
      ['paused', 'The agent is paused, so no review starts now.'],
      ['credentials_invalid', 'The agent has lost access to your files, so no review starts now. Reconnect it.'],
      ['quiet_hours', 'It is quiet hours now (UTC), so no review starts on its own.'],
      ['live_job', 'A review is still waiting, working or waiting for your approval; the next one starts after it ends.'],
      ['daily_cap', "Today's limit of reviews is reached; reviews resume at midnight UTC."],
      ['budget', "Most of today's token budget is used; reviews resume at midnight UTC."],
      ['<b>new</b>', 'No review can start right now.']
    ])('explains why no review starts now (%s) with a fixed text', async (blocked, text) => {
      const { w } = await setup(settingsServer({ proactive_enabled: true }, { proactive: proactive({ blocked }) }).handler)
      expect(w.find('[data-testid="agent-proactive-blocked"]').text()).toBe(text)
    })

    it('clears quiet hours with an empty value', async () => {
      const s = settingsServer({ quiet_hours: '22:00-07:00' })
      const { w } = await setup(s.handler)
      await input(w, 'agent-quiet-hours').setValue('')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"quiet_hours":""}]')
    })
  })

  describe('saving', () => {
    it('sends exactly the changed fields together and shows the answer', async () => {
      const s = settingsServer()
      const { w, platform } = await setup(s.handler)
      await w.find('[data-testid="agent-proactive"] [role="switch"]').trigger('click')
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await choose(w, 'agent-model-proactive', 'Model B (m2)')
      await save(w)
      expect(JSON.stringify(s.put)).toBe(
        '[{"proactive_enabled":true,"quiet_hours":"22:00-07:00","models":{"proactive":"m2"}}]'
      )
      expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
      expect(platform.notify).toHaveBeenCalledWith('success', 'Settings saved')
    })

    it('shows what the server stored, not what was typed', async () => {
      const s = settingsServer({}, {}, (c) =>
        c.method === 'PUT' ? json(settingsOf({ watch: ['/a', '/normalised'], daily_tokens: 50000 })) : undefined
      )
      const { w } = await setup(s.handler)
      await input(w, 'agent-watch-new').setValue('/typed')
      await button(w, 'Add')!.trigger('click')
      await save(w)
      expect(w.findAll('[data-testid="agent-watch-item"]').map((e) => e.find('pre, code').text())).toEqual([
        '/a',
        '/normalised'
      ])
    })

    it('keeps the form and shows a generic message on invalid_request', async () => {
      const s = settingsServer({}, {}, (c) => (c.method === 'PUT' ? apiErr(400, 'invalid_request') : undefined))
      const { w } = await setup(s.handler)
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await save(w)
      expect(w.find('[data-testid="agent-save-error"]').text()).toBe(
        'The request was not accepted. Check your input and try again.'
      )
      expect(w.text()).not.toContain('server text')
      expect(input(w, 'agent-quiet-hours').element.value).toBe('22:00-07:00')
      expect(button(w, 'Save')!.attributes('disabled')).toBeUndefined()
    })

    it('shows the retry time when rate limited', async () => {
      const s = settingsServer({}, {}, (c) =>
        c.method === 'PUT'
          ? new Response(JSON.stringify({ error: { code: 'rate_limited', message: 'x' } }), {
              status: 429,
              headers: { 'Retry-After': '7', 'Content-Type': 'application/json' }
            })
          : undefined
      )
      const { w } = await setup(s.handler)
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await save(w)
      expect(w.find('[data-testid="agent-save-error"]').text()).toContain('Try again in 7 seconds.')
    })

    it('never sends twice on a double click', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const s = settingsServer()
      const { w, server } = await setup((c) => (c.method === 'PUT' ? gate.then(() => s.handler(c)!) : s.handler(c)))
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await button(w, 'Save')!.trigger('click')
      expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
      await button(w, 'Save')!.trigger('click')
      release()
      await flushPromises()
      expect(server.count('PUT', '/settings')).toBe(1)
    })

    it('ignores the proactive switch while saving', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const s = settingsServer()
      const { w } = await setup((c) => (c.method === 'PUT' ? gate.then(() => s.handler(c)!) : s.handler(c)))
      const sw = () => w.find('[data-testid="agent-proactive"] [role="switch"]')
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await button(w, 'Save')!.trigger('click')
      await sw().trigger('click')
      expect(sw().attributes('aria-checked')).toBe('false')
      release()
      await flushPromises()
      expect(JSON.stringify(s.put)).toBe('[{"quiet_hours":"22:00-07:00"}]')
      expect(sw().attributes('aria-checked')).toBe('false')
    })

    it('keeps an emptied daily limit across a refresh', async () => {
      const s = settingsServer()
      const { w, store } = await setup(s.handler)
      await input(w, 'agent-daily-tokens').setValue('')
      await store.refreshSettings()
      await flushPromises()
      expect(input(w, 'agent-daily-tokens').element.value).toBe('')
      expect(fieldError(w, 'agent-daily-tokens')).not.toBeNull()
      expect(button(w, 'Discard changes')!.attributes('disabled')).toBeUndefined()
      expect(w.text()).not.toContain('The settings were changed elsewhere')
    })

    it('a refresh with the same settings while editing shows no note', async () => {
      const s = settingsServer()
      const { w, store } = await setup(s.handler)
      await w.find('[data-testid="agent-proactive"] [role="switch"]').trigger('click')
      await store.refreshSettings()
      await flushPromises()
      expect(w.text()).not.toContain('The settings were changed elsewhere')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"proactive_enabled":true}]')
    })

    it('does not save a change that sends nothing', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-daily-tokens').setValue('050000')
      expect(button(w, 'Discard changes')!.attributes('disabled')).toBeUndefined()
      expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
      await save(w)
      expect(s.put).toHaveLength(0)
    })

    it('discards the changes', async () => {
      const s = settingsServer()
      const { w } = await setup(s.handler)
      await input(w, 'agent-quiet-hours').setValue('22:00-07:00')
      await input(w, 'agent-watch-new').setValue('/x')
      await button(w, 'Add')!.trigger('click')
      await button(w, 'Discard changes')!.trigger('click')
      expect(input(w, 'agent-quiet-hours').element.value).toBe('')
      expect(w.findAll('[data-testid="agent-watch-item"]')).toHaveLength(1)
      expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
    })

    it('follows changes made elsewhere while the form is untouched', async () => {
      const s = settingsServer()
      const { w, store } = await setup(s.handler)
      s.set({ quiet_hours: '01:00-02:00' })
      await store.refreshSettings()
      await flushPromises()
      expect(input(w, 'agent-quiet-hours').element.value).toBe('01:00-02:00')
    })

    it('keeps the edits and says so when the settings change elsewhere', async () => {
      const s = settingsServer()
      const { w, store } = await setup(s.handler)
      await w.find('[data-testid="agent-proactive"] [role="switch"]').trigger('click')
      s.set({ quiet_hours: '01:00-02:00' })
      await store.refreshSettings()
      await flushPromises()
      expect(input(w, 'agent-quiet-hours').element.value).toBe('')
      expect(w.text()).toContain('The settings were changed elsewhere')
      await save(w)
      expect(JSON.stringify(s.put)).toBe('[{"proactive_enabled":true}]')
      expect(input(w, 'agent-quiet-hours').element.value).toBe('01:00-02:00')
    })
  })
})
