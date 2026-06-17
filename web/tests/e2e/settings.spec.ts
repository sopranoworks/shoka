import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// B-28 stage 3 — the Settings rail mode + user management + redeem, in a real browser.
// Its own server (passkeys on via rp_id=localhost, first-run on, empty store), seeded
// with a nested project BEFORE the first admin exists (while /ws/ui is the no-lockout
// super-user pass-through). Covers the directive's E2E cores: the gear is always
// present; Settings shows the (super-user) user-management item; switching
// Explorer→Settings→Explorer does NOT collapse the tree (74a7c8c intact); and the
// invite → logout → redeem flow creates a scoped account.

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..')
const PORT = 8092
const MCP_PORT = 8091
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
  throw new Error(`settings server did not become ready at ${url}`)
}

// Seed a nested project over /ws/ui while the user store is EMPTY (super-user
// pass-through), so the tree has an expandable folder before the admin exists.
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
  await rpc(ws, 'SAVE_FILE', { namespace: 'demo', projectName: 'docs', path: 'guides/intro.md', content: '# Intro\n' })
  ws.close()
}

test.beforeAll(async () => {
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-settings-'))
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

// Ensure an admin session: each test gets a fresh browser context (no cookies), and
// the server is shared — so the FIRST test sees the first-run wizard, later tests see
// the login screen. Branch on whichever appears. Password-only (the passkey offer is
// dismissed); the passkey ceremony itself is proven in passkey.spec.ts.
async function loginOrRegister(page: Page, dialogs: { accept: boolean }) {
  page.on('dialog', (d) => (dialogs.accept ? d.accept() : d.dismiss()))
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
    await page.locator('#lg-email').fill('admin@example.com')
    await page.locator('#lg-pw').fill('hunter2hunter2')
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(login).toBeHidden({ timeout: 15000 })
  }
}

test('Settings rail mode: gear present, user-management visible, no tree collapse', async ({ page }) => {
  const dialogs = { accept: false } // dismiss the first-run passkey offer (password-only admin)
  await loginOrRegister(page, dialogs)

  // Open the project and its tree.
  await page.goto(`${BASE}/p/demo/docs`)
  // Expand the "guides" folder so a child becomes visible.
  await page.getByText('guides', { exact: true }).click()
  await expect(page.getByText('intro.md')).toBeVisible()

  // The gear is present; activate Settings.
  await page.getByRole('button', { name: 'Settings' }).click()
  // The settings item list shows BOTH super-user items (User management + OAuth).
  await expect(page.getByRole('link', { name: 'User management' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'OAuth connections' })).toBeVisible()
  await page.getByRole('link', { name: 'User management' }).click()
  await expect(page.getByRole('heading', { name: 'User management' })).toBeVisible()
  // Selecting the OAuth item renders the existing Connections screen in the right pane.
  await page.getByRole('link', { name: 'OAuth connections' }).click()
  await expect(page.getByRole('button', { name: 'Refresh connections' })).toBeVisible()

  // The third item — Namespace / project management (B-28 part 2) — is visible to the
  // super-user admin and renders the managed namespace→projects listing (the real
  // NAMESPACE_HEALTH path) with the seeded demo/docs project.
  await page.getByRole('link', { name: 'Namespace / project management' }).click()
  await expect(page.getByRole('heading', { name: 'Namespace / project management' })).toBeVisible()
  const demoBlock = page.getByTestId('ns-demo')
  await expect(demoBlock).toBeVisible()
  await expect(demoBlock.getByText('docs', { exact: true })).toBeVisible()

  // Back to Explorer: the tree did NOT collapse — "intro.md" is still visible
  // (the ProjectTree stayed mounted while hidden in Settings; 74a7c8c intact).
  await page.getByRole('button', { name: 'Explorer' }).click()
  await expect(page.getByText('intro.md')).toBeVisible()
})

test('invite → logout → redeem creates a scoped account', async ({ page }) => {
  const dialogs = { accept: false }
  await loginOrRegister(page, dialogs)

  // As admin, open user management and create an invite.
  await page.goto(`${BASE}/settings?item=users`)
  await expect(page.getByRole('heading', { name: 'User management' })).toBeVisible()
  await page.getByLabel('invitee email').fill('invitee@example.com')
  // The default grant row needs a namespace; type "demo".
  await page.getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()
  expect(code.length).toBeGreaterThan(10)

  // Log out (drop the session) and redeem off the login screen.
  await page.context().clearCookies()
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible()
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeVisible()
  await page.locator('#rd-pw').fill('inviteepw123')
  await page.locator('#rd-pw2').fill('inviteepw123')
  await page.getByRole('button', { name: 'Create account' }).click()

  // Redeemed → authenticated as the scoped invitee.
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })
  const status = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(status.authenticated).toBe(true)
  expect(status.principal.email).toBe('invitee@example.com')
  expect(status.principal.is_admin).toBe(false)
})
