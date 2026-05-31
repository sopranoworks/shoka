import { defineConfig, devices } from '@playwright/test'

// E2E runs against a REAL Shoka binary serving the production bundle, exercising
// the actual /ws/ui request/response path and the Go SPA-fallback static serve.
// tests/e2e/global-setup.ts builds the binary, seeds a fixture data dir over
// /ws/ui, starts the server on PORT, and returns a teardown. The tests assert
// the v3 §2 calibre properties (URL-as-state, deep-link boot, palette,
// responsive). Run via `npm run test:e2e` (builds the bundle first).
const PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099)

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  reporter: 'list',
  globalSetup: './tests/e2e/global-setup.ts',
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: 'on-first-retry',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
