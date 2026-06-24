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
  // 'list' for the live log + the failure-evidence archiver (directive §2.5): on ANY
  // failure it copies the complete failing moment (trace/video/screenshot + replay inputs)
  // OUT to a non-wiped per-run archive before the next run clears test-results — so a flake
  // that fires on run 7 or run 700 is always fully reconstructable afterward.
  reporter: [['list'], ['./tests/e2e/failure-archiver.ts']],
  globalSetup: './tests/e2e/global-setup.ts',
  use: {
    baseURL: `http://localhost:${PORT}`,
    // Retain on failure (NOT on-first-retry, which never fires at retries:0): the trace
    // carries DOM snapshots + console + network + step timing; video + screenshot complete
    // the moment. The archiver copies them out before test-results is wiped.
    trace: 'retain-on-failure',
    video: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{
    name: 'chromium',
    use: {
      ...devices['Desktop Chrome'],
      launchOptions: {
        executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ||
          undefined,
      },
    },
  }],
})
