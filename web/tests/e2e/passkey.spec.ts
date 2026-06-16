import { test, expect } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// B-28 stage 1 — the real-browser passkey ceremony. WebAuthn needs a genuine secure
// context, so this MUST run in a real browser with a virtual authenticator (a jsdom
// unit test cannot — the trash-DnD lesson). It stands up its OWN Shoka server with
// passkeys enabled (rp_id=localhost, the secure-context dev origin) and an EMPTY user
// store so the first-run wizard appears, then drives:
//   register: first-run wizard -> create admin -> enrol a passkey (attestation);
//   assert:   drop the session -> login screen -> sign in with the passkey (assertion).

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..')
const PORT = 8090
const MCP_PORT = 8089
const BASE = `http://localhost:${PORT}`

let server: ChildProcess | null = null
let dataDir = ''

async function waitForHttp(url: string, timeoutMs = 20000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 200))
  }
  throw new Error(`passkey server did not become ready at ${url}`)
}

test.beforeAll(async () => {
  // Reuse the binary built by global-setup (it embeds the freshly-built bundle).
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-passkey-'))
  const cfgPath = join(dataDir, 'shoka.yaml')
  writeFileSync(
    cfgPath,
    [
      'server:',
      '  http:',
      `    listen: ":${PORT}"`,
      '  mcp:',
      '    plain:',
      `      listen: ":${MCP_PORT}"`,
      '  log:',
      '    level: "warn"',
      '  auth:',
      '    enabled: false',
      // Passkeys ON: rp_id=localhost is a valid secure-context RP ID (the dev case).
      // First-run wizard enabled (default) so the empty store shows it.
      '    webauthn:',
      '      rp_id: "localhost"',
      '      rp_display_name: "Shoka Test"',
      '      rp_origins:',
      `        - "${BASE}"`,
      'storage:',
      `  base_dir: "${join(dataDir, 'data')}"`,
      '  drift_scan:',
      '    on_startup: true',
      '    interval: 0',
      '',
    ].join('\n'),
  )
  const logFd = openSync(join(dataDir, 'server.log'), 'w')
  server = spawn(binPath, ['--config', cfgPath], { stdio: ['ignore', logFd, logFd] })
  await waitForHttp(`${BASE}/`)
})

test.afterAll(() => {
  server?.kill('SIGTERM')
  if (dataDir) rmSync(dataDir, { recursive: true, force: true })
})

test('first-run enrols a passkey, then a fresh session logs in by passkey assertion', async ({ page }) => {
  // A CDP virtual authenticator satisfies navigator.credentials.create/get without
  // real hardware or a user gesture.
  const client = await page.context().newCDPSession(page)
  await client.send('WebAuthn.enable')
  await client.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  })
  // The wizard offers the passkey via window.confirm — accept it.
  page.on('dialog', (d) => d.accept())

  // --- register ---
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Welcome to Shoka' })).toBeVisible()
  await page.locator('#fr-email').fill('admin@example.com')
  await page.locator('#fr-name').fill('Admin')
  await page.locator('#fr-pw').fill('hunter2hunter2')
  await page.locator('#fr-pw2').fill('hunter2hunter2')
  await page.getByRole('button', { name: /Create administrator/ }).click()

  // The wizard is replaced by the app, and the session is authenticated with a
  // passkey enrolled.
  await expect(page.getByRole('heading', { name: 'Welcome to Shoka' })).toBeHidden({ timeout: 15000 })
  const status = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(status.authenticated).toBe(true)
  expect(status.principal.email).toBe('admin@example.com')
  expect(status.principal.is_admin).toBe(true)

  // --- assert (passwordless login) ---
  await page.context().clearCookies()
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible()
  await page.locator('#lg-email').fill('admin@example.com')
  await page.getByRole('button', { name: 'Sign in with a passkey' }).click()

  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeHidden({ timeout: 15000 })
  const status2 = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(status2.authenticated).toBe(true)
  expect(status2.principal.email).toBe('admin@example.com')
})
