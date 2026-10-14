// @ownclouders/extension-sdk ships plain ESM without type declarations.
declare module '@ownclouders/extension-sdk' {
  import type { UserConfig } from 'vite'
  export function defineConfig(overrides?: UserConfig & { name?: string }): UserConfig
}
