import { hasInjectionContext, inject, provide, type App, type InjectionKey } from 'vue'

// Platform is the only way components and stores reach the oCIS Web runtime
// and the browser, so they can be tested without either. getToken reads the
// current bearer at call time; callers must never keep the value.
export interface Platform {
  getToken(): string | undefined
  backendUrl: string
  fetch: typeof fetch
  notify(kind: 'success' | 'error' | 'info', title: string, desc?: string): void
  confirm(opts: {
    title: string
    message: string
    confirmText: string
    danger?: boolean
  }): Promise<boolean>
  now(): number
  random(): number
}

export const PLATFORM_KEY: InjectionKey<Platform> = Symbol('ocis-agent-platform')

// providePlatform provides p on an app, or, with a null target, from the
// setup() of the calling component.
export const providePlatform = (target: Pick<App, 'provide'> | null, p: Platform): void => {
  if (target) {
    target.provide(PLATFORM_KEY, p)
  } else {
    provide(PLATFORM_KEY, p)
  }
}

export const usePlatform = (): Platform => {
  const p = hasInjectionContext() ? inject(PLATFORM_KEY, null) : null
  if (!p) {
    throw new Error('ocis-agent: no platform provided (call providePlatform first)')
  }
  return p
}

const DEFAULT_BACKEND_URL = '/agent-api/v1'

// PlatformDeps are the pieces of the oCIS Web runtime the real platform uses.
export interface PlatformDeps {
  accessToken: () => string | undefined
  backendUrl: unknown
  fetch: typeof fetch
  showMessage: (m: { title: string; desc?: string; status?: string }) => unknown
  showErrorMessage: (m: { title: string; desc?: string }) => unknown
  dispatchModal: (m: {
    title: string
    message: string
    confirmText: string
    variation: string
    onConfirm: () => void
    onCancel: () => void
  }) => unknown
}

// createPlatform builds the real platform from the oCIS Web runtime.
export const createPlatform = (d: PlatformDeps): Platform => {
  const configured = typeof d.backendUrl === 'string' ? d.backendUrl.replace(/\/+$/, '') : ''
  return {
    getToken: () => d.accessToken() || undefined,
    backendUrl: configured || DEFAULT_BACKEND_URL,
    fetch: d.fetch,
    notify: (kind, title, desc) => {
      const m = { title, ...(desc !== undefined && { desc }) }
      if (kind === 'error') d.showErrorMessage(m)
      else d.showMessage(kind === 'info' ? { ...m, status: 'passive' } : m)
    },
    confirm: (opts) =>
      new Promise<boolean>((resolve) => {
        d.dispatchModal({
          title: opts.title,
          message: opts.message,
          confirmText: opts.confirmText,
          variation: opts.danger ? 'danger' : 'passive',
          onConfirm: () => resolve(true),
          onCancel: () => resolve(false)
        })
      }),
    now: () => Date.now(),
    random: () => Math.random()
  }
}
