/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'

const webCoreDir = fileURLToPath(new URL('../packages/web-core/src', import.meta.url))

export default defineConfig({
  base: '/',
  plugins: [react()],
  resolve: {
    alias: {
      '@shoka/web-core/tokens.css': `${webCoreDir}/styles/tokens.css`,
      '@shoka/web-core/pages/SettingsPage': `${webCoreDir}/pages/SettingsPage.tsx`,
      '@shoka/web-core': `${webCoreDir}/index.ts`,
    },
  },
  build: {
    outDir: '../server/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/ws/ui': { target: 'ws://localhost:8080', ws: true },
      '/api': { target: 'http://localhost:8080' },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: true,
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
  },
})
