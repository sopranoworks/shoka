/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// This source uses CODE-BASED routing (src/router.tsx), not TanStack Router's
// file-based route tree, so the @tanstack/router-plugin is intentionally NOT
// used. Code-based routing keeps the whole route tree in one typed file.
//
// base: '/' — absolute asset paths so a deep-link SPA boot (e.g. opening
// /p/shoka/maintenance/blob/backlog.md directly) resolves its JS/CSS from the
// domain root rather than against the deep URL. Shoka serves the UI only at the
// domain root, so absolute-at-root is correct; the prototype's base:'./' would
// 404 its assets at depth (see README "Asset base"). This is the v3 §0 fix.
//
// build.outDir -> ../server/dist, the directory the Go server embeds
// (server/embed.go: //go:embed dist). Go's embed pattern cannot use "..", so
// the build writes into server/dist directly, exactly as the old web/ did.
//
// server.proxy forwards the dev server's /ws/ui and /api to a locally running
// Shoka binary (default HTTP listen :8080), so `npm run dev` hot-reloads the UI
// while talking to the real backend.
export default defineConfig({
  base: '/',
  plugins: [react()],
  resolve: {
    alias: {
      '@shoka/web-core/tokens.css': '../packages/web-core/src/styles/tokens.css',
      '@shoka/web-core/pages/SettingsPage': '../packages/web-core/src/pages/SettingsPage.tsx',
      '@shoka/web-core': '../packages/web-core/src/index.ts',
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
