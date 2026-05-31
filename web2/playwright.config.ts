import { defineConfig, devices } from '@playwright/test'

// E2E runs against a REAL Shoka binary serving the production bundle, exercising
// the actual /ws/ui request/response path and the Go SPA-fallback static serve.
// global-setup.ts builds the bundle + binary, seeds a fixture data dir, and
// starts the server on PORT; global-teardown.ts stops it. The tests assert the
// v3 §2 calibre properties (URL-as-state, deep-link boot, palette, responsive).
const PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099)

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: 'list',
  globalSetup: './tests/e2e/global-setup.ts',
  globalTeardown: './tests/e2e/global-teardown.ts',
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: 'on-first-retry',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
