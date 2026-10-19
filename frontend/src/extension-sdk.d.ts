// @ownclouders/extension-sdk ships plain ESM without type declarations.
declare module '@ownclouders/extension-sdk' {
  import type { UserConfig } from 'vitest/config'
  export function defineConfig(overrides?: UserConfig & { name?: string }): UserConfig
}
