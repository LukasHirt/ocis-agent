import { defineConfig } from '@ownclouders/extension-sdk'

export default defineConfig({
  name: 'web-app-agent',
  build: {
    rollupOptions: {
      output: {
        // stable file name so manifest.json's entrypoint is valid
        entryFileNames: 'js/[name].js'
      }
    }
  },
  test: {
    setupFiles: ['./vitest.setup.ts']
  }
})
