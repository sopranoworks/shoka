import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// B-28 — the self-service "My Account" Settings page, in a real browser, reached the
// way a user reaches it (the Settings gear → the My Account item), NOT page.goto. Its
// own server (first-run wizard on, empty store), seeded with a demo/docs project so an
// invite can grant a namespace and the app renders. Covers the directive's E2E cores:
// a NORMAL non-admin user views their info (email read-only), changes their name (it
// persists across a reload), and resets their password (re-login with the NEW one
// succeeds; the OLD one fails) — plus visibility (a non-admin sees My Account but NOT
// the admin-only pages; an admin sees all).

const PORT = 8088
const MCP_PORT = 8087
const BASE = `http://localhost:${PORT}`

const ADMIN_EMAIL = 'admin@example.com'
const ADMIN_PW = 'hunter2hunter2'
const USER_EMAIL = 'user@example.com'
const USER_PW = 'origpw12345'
const USER_NEW_PW = 'newpw123456'

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
  throw new Error(`my-account server did not become ready at ${url}`)
}

// Seed a nested project over /ws/ui while the user store is EMPTY (super-user
// pass-through), so an invite can grant the "demo" namespace and the app renders.
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
  const binPath = join(tmpdir(), 'shoka-e2e-bin') // built by global-setup, embeds the fresh bundle
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-account-'))
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

// Register the first admin (only the very first call sees the wizard) OR sign in.
async function ensureAdmin(page: Page) {
  page.on('dialog', (d) => d.dismiss()) // dismiss the first-run passkey offer (password-only)
  await page.goto(BASE)
  const wizard = page.getByRole('heading', { name: 'Welcome to Shoka' })
  const login = page.getByRole('heading', { name: 'Sign in to Shoka' })
  await expect(wizard.or(login)).toBeVisible({ timeout: 15000 })
  if (await wizard.isVisible()) {
    await page.locator('#fr-email').fill(ADMIN_EMAIL)
    await page.locator('#fr-pw').fill(ADMIN_PW)
    await page.locator('#fr-pw2').fill(ADMIN_PW)
    await page.getByRole('button', { name: /Create administrator/ }).click()
    await expect(wizard).toBeHidden({ timeout: 15000 })
  } else {
    await page.locator('#lg-email').fill(ADMIN_EMAIL)
    await page.locator('#lg-pw').fill(ADMIN_PW)
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(login).toBeHidden({ timeout: 15000 })
  }
}

// Sign in (password) as an existing account; expects the login screen to be present.
async function signIn(page: Page, email: string, pw: string) {
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible({ timeout: 15000 })
  await page.locator('#lg-email').fill(email)
  await page.locator('#lg-pw').fill(pw)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
}

// Create the non-admin user via the real invite→redeem flow (idempotent-ish: the
// caller runs it once). Leaves the browser logged in AS the new user.
async function inviteAndRedeemUser(page: Page) {
  await ensureAdmin(page)
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('link', { name: 'User management' }).click()
  await expect(page.getByRole('heading', { name: 'User management' })).toBeVisible()
  await page.getByLabel('invitee email').fill(USER_EMAIL)
  await page.getByRole('form', { name: 'Create invite' }).getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()

  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeVisible()
  await page.locator('#rd-pw').fill(USER_PW)
  await page.locator('#rd-pw2').fill(USER_PW)
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })
}

// Reach My Account through the gear (NOT a deep page.goto).
async function openMyAccount(page: Page) {
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('link', { name: 'My Account' }).click()
  await expect(page.getByRole('heading', { name: 'My Account' })).toBeVisible()
}

test('a normal non-admin user: view → change name (persists) → reset password → re-login', async ({ page }) => {
  await inviteAndRedeemUser(page) // now logged in as the non-admin USER_EMAIL

  // VIEW: own info — email (read-only, NOT an editable field), name, role.
  await openMyAccount(page)
  await expect(page.getByText(USER_EMAIL).first()).toBeVisible()
  await expect(page.getByText('your account ID; it cannot be changed')).toBeVisible()
  // Email is immutable in the UI: there is no textbox carrying the email.
  await expect(page.getByRole('textbox', { name: /email/i })).toHaveCount(0)
  await expect(page.getByText('Standard user')).toBeVisible()

  // CHANGE NAME → it persists across a reload.
  const nameField = page.getByLabel('Display name')
  await nameField.fill('Renamed User')
  await page.getByRole('button', { name: 'Save name' }).click()
  await page.reload()
  await expect(page.getByLabel('Display name')).toHaveValue('Renamed User', { timeout: 15000 })

  // RESET PASSWORD (current + new + confirm).
  await page.getByLabel('Current password').fill(USER_PW)
  await page.getByLabel('New password', { exact: true }).fill(USER_NEW_PW)
  await page.getByLabel('Confirm new password').fill(USER_NEW_PW)
  await page.getByRole('button', { name: 'Change password' }).click()
  // Let the op round-trip (button re-enables / form clears on success).
  await expect(page.getByLabel('Current password')).toHaveValue('', { timeout: 15000 })

  // Sign out; the NEW password works.
  await page.context().clearCookies()
  await signIn(page, USER_EMAIL, USER_NEW_PW)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeHidden({ timeout: 15000 })
  const okStatus = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(okStatus.authenticated).toBe(true)
  expect(okStatus.principal.email).toBe(USER_EMAIL)

  // Sign out; the OLD password FAILS (no session established).
  await page.context().clearCookies()
  await signIn(page, USER_EMAIL, USER_PW)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible()
  const failStatus = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(failStatus.authenticated).toBe(false)
})

test('visibility: a non-admin sees My Account but NOT the admin-only pages; an admin sees all', async ({ page }) => {
  // Non-admin (created in the first test; sign in with the NEW password).
  await signIn(page, USER_EMAIL, USER_NEW_PW)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeHidden({ timeout: 15000 })
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await expect(page.getByRole('link', { name: 'My Account' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'User management' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'OAuth connections' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Namespace / project management' })).toHaveCount(0)

  // Admin sees My Account AND the admin-only items.
  await page.context().clearCookies()
  await ensureAdmin(page)
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await expect(page.getByRole('link', { name: 'My Account' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'User management' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'OAuth connections' })).toBeVisible()
})
