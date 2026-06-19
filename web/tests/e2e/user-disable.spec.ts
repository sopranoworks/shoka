import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// B-28 user enable/disable + access revocation, in a real browser through the super-user
// UI path. Its own server (passkeys on via rp_id=localhost, first-run on, empty store),
// seeded with demo/docs so the activity-rail gear / navigation works. Covers:
//   - the disabled-state column + toggle (Active <-> Disabled) reached via the gear, not goto;
//   - self omitted from the user list (no self-toggle);
//   - the WebAuthn login gate: a disabled user holding a passkey cannot complete passkey login.
// The password login gate + the oauth-revoke + the self-guard are unit-tested in Go.

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..')
const PORT = 8085
const MCP_PORT = 8084
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
  throw new Error(`user-disable server did not become ready at ${url}`)
}

// Seed demo/docs over /ws/ui while the store is empty (super-user pass-through), so the
// app shell and the gear are reachable before any admin exists.
async function seed(): Promise<void> {
  const rpc = (ws: WebSocket, type: string, payload: unknown) =>
    new Promise<void>((resolve, reject) => {
      const onMsg = (ev: MessageEvent) => {
        const m = JSON.parse(String(ev.data)) as { type: string }
        if (m.type === 'NOTIFY') return
        ws.removeEventListener('message', onMsg)
        m.type === 'ERROR' ? reject(new Error('seed error')) : resolve()
      }
      ws.addEventListener('message', onMsg)
      ws.send(JSON.stringify({ type, payload }))
    })
  const ws = new WebSocket(`ws://localhost:${PORT}/ws/ui`)
  await new Promise<void>((resolve, reject) => {
    ws.addEventListener('open', () => resolve())
    ws.addEventListener('error', () => reject(new Error('seed ws connect failed')))
  })
  await rpc(ws, 'CREATE_PROJECT', { namespace: 'demo', projectName: 'docs' })
  await rpc(ws, 'SAVE_FILE', { namespace: 'demo', projectName: 'docs', path: 'intro.md', content: '# Intro\n' })
  ws.close()
}

test.beforeAll(async () => {
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-userdisable-'))
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
  await seed()
})

test.afterAll(() => {
  server?.kill('SIGTERM')
  if (dataDir) rmSync(dataDir, { recursive: true, force: true })
})

// Ensure the admin session exists: the first test sees the first-run wizard, later tests
// the login screen. Password-only (the passkey offer is dismissed via the shared dialog
// handler the caller installs).
async function ensureAdmin(page: Page) {
  await page.goto(BASE)
  const wizard = page.getByRole('heading', { name: 'Welcome to Shoka' })
  const login = page.getByRole('heading', { name: 'Sign in to Shoka' })
  await expect(wizard.or(login)).toBeVisible({ timeout: 15000 })
  if (await wizard.isVisible()) {
    await page.locator('#fr-email').fill('admin@example.com')
    await page.locator('#fr-pw').fill('hunter2hunter2')
    await page.locator('#fr-pw2').fill('hunter2hunter2')
    await page.getByRole('button', { name: /Create administrator/ }).click()
    await expect(wizard).toBeHidden({ timeout: 15000 })
  } else {
    await loginAdmin(page)
  }
}

async function loginAdmin(page: Page) {
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible({ timeout: 15000 })
  await page.locator('#lg-email').fill('admin@example.com')
  await page.locator('#lg-pw').fill('hunter2hunter2')
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeHidden({ timeout: 15000 })
}

// Reach user management through the gear (the super-user path), not a deep goto.
async function openUserMgmt(page: Page) {
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('link', { name: 'User management' }).click()
  await expect(page.getByRole('heading', { name: 'User management' })).toBeVisible()
}

async function createInviteCode(page: Page, email: string): Promise<string> {
  await openUserMgmt(page)
  await page.getByLabel('invitee email').fill(email)
  await page.getByRole('form', { name: 'Create invite' }).getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  return page.locator('code').first().innerText()
}

test('enable/disable: toggle a user disabled and back, through the UI; self omitted', async ({ page }) => {
  page.on('dialog', (d) => d.dismiss()) // password-only: dismiss any passkey offer
  await ensureAdmin(page)
  const code = await createInviteCode(page, 'u2@example.com')

  // Redeem the invite (password-only) to create the second user.
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.locator('#rd-pw').fill('u2password12')
  await page.locator('#rd-pw2').fill('u2password12')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })

  // Back to the admin; open user management via the gear.
  await page.context().clearCookies()
  await loginAdmin(page)
  await openUserMgmt(page)

  // Self is omitted from the list — there is no self row to toggle.
  await expect(page.getByRole('cell', { name: 'admin@example.com' })).toHaveCount(0)

  const row = page.getByRole('row', { name: /u2@example.com/ })
  await expect(row.getByTestId('user-state')).toHaveText('Active')
  await row.getByRole('button', { name: 'Disable user' }).click()
  await expect(row.getByTestId('user-state')).toHaveText('Disabled')
  await row.getByRole('button', { name: 'Enable user' }).click()
  await expect(row.getByTestId('user-state')).toHaveText('Active')
})

test('login gate: a disabled user with a passkey cannot complete passkey login', async ({ page }) => {
  // A CDP virtual authenticator satisfies the WebAuthn ceremony without real hardware.
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
  let acceptPasskey = false
  page.on('dialog', (d) => (acceptPasskey ? d.accept() : d.dismiss()))

  await ensureAdmin(page) // admin already exists (shared server) → password login
  const code = await createInviteCode(page, 'pk@example.com')

  // Redeem WITH a passkey: accept the "set up a passkey?" confirm so a credential is
  // enrolled in the virtual authenticator.
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.locator('#rd-pw').fill('pkpassword12')
  await page.locator('#rd-pw2').fill('pkpassword12')
  acceptPasskey = true
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })
  acceptPasskey = false

  // Admin disables the passkey user.
  await page.context().clearCookies()
  await loginAdmin(page)
  await openUserMgmt(page)
  const row = page.getByRole('row', { name: /pk@example.com/ })
  await row.getByRole('button', { name: 'Disable user' }).click()
  await expect(row.getByTestId('user-state')).toHaveText('Disabled')

  // The disabled user attempts passkey login → refused at the WebAuthn finish gate.
  await page.context().clearCookies()
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible()
  await page.locator('#lg-email').fill('pk@example.com')
  await page.getByRole('button', { name: 'Sign in with a passkey' }).click()
  // Stays on the login screen, not authenticated.
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible({ timeout: 5000 })
  const status = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(status.authenticated).toBe(false)
})
