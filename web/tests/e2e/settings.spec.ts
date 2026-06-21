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

// ---------------------------------------------------------------------------
// 2026-06-19 user-management UI: two bug fixes + two improvements (frontend).
// Each reaches the page the way a super-user does — log in, click the Settings
// gear, click the User management item — NOT a page.goto to a deep route.
// ---------------------------------------------------------------------------

// Reach user management through the activity-rail gear (the super-user path), not a
// deep settings route.
async function openUserManagement(page: Page) {
  await page.goto(`${BASE}/p/demo/docs`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('link', { name: 'User management' }).click()
  await expect(page.getByRole('heading', { name: 'User management' })).toBeVisible()
}

// Create a non-super-user via the real invite→redeem flow, then return to the admin
// session — so the user list is non-empty and the per-user ScopeEditor can be opened.
async function createSecondUser(page: Page, email: string) {
  await openUserManagement(page)
  await page.getByLabel('invitee email').fill(email)
  await page.getByRole('form', { name: 'Create invite' }).getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeVisible()
  await page.locator('#rd-pw').fill('secondpw1234')
  await page.locator('#rd-pw2').fill('secondpw1234')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })
  // Back to the admin session.
  await page.context().clearCookies()
  await loginOrRegister(page, { accept: false })
}

test('Bug 1: "All namespaces" replaces the per-namespace rows in the invite form', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  const form = page.getByRole('form', { name: 'Create invite' })
  await form.getByLabel('namespace').first().fill('demo')
  await form.getByRole('button', { name: /Add namespace/ }).click()
  await form.getByLabel('namespace').nth(1).fill('other')
  await expect(form.getByLabel('namespace')).toHaveCount(2)
  await form.getByRole('button', { name: /Wildcard/ }).click()
  // Replace-on-add: only the single wildcard row remains, the individual rows are gone.
  await expect(form.getByLabel('namespace')).toHaveCount(0)
  await expect(form.getByText('All namespaces (*)')).toBeVisible()
})

test('Bug 1: "All namespaces" replaces the per-namespace rows in the scope editor', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await createSecondUser(page, 'scopeuser@example.com')
  await openUserManagement(page)
  await page
    .getByRole('row', { name: /scopeuser@example.com/ })
    .getByRole('button', { name: 'Edit permissions' })
    .click()
  const editor = page.getByLabel('Scope editor')
  await expect(editor.getByLabel('namespace')).toHaveCount(1)
  await editor.getByRole('button', { name: /Add namespace/ }).click()
  await expect(editor.getByLabel('namespace')).toHaveCount(2)
  await editor.getByRole('button', { name: /Wildcard/ }).click()
  await expect(editor.getByLabel('namespace')).toHaveCount(0)
  await expect(editor.getByText('All namespaces (*)')).toBeVisible()
  // Bug B (editor): with a wildcard present, "+ Add namespace" is hidden too — no
  // individual rows can be added back alongside the wildcard.
  await expect(editor.getByRole('button', { name: /Add namespace/ })).toHaveCount(0)
})

test('Bug A: "Generate invite code" re-disables when the email is cleared', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  const form = page.getByRole('form', { name: 'Create invite' })
  const gen = page.getByRole('button', { name: 'Generate invite code' })
  // With a namespace filled, only the email gates the button.
  await form.getByLabel('namespace').first().fill('demo')
  await expect(gen).toBeDisabled() // email still empty
  await page.getByLabel('invitee email').fill('a@example.com')
  await expect(gen).toBeEnabled()
  // Clearing the email returns the button to disabled (the bug: it stayed enabled).
  await page.getByLabel('invitee email').fill('')
  await expect(gen).toBeDisabled()
})

test('Bug B: "+ Add namespace" is hidden while a wildcard grant is present', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  const form = page.getByRole('form', { name: 'Create invite' })
  await expect(form.getByRole('button', { name: /Add namespace/ })).toBeVisible()
  await form.getByRole('button', { name: /Wildcard/ }).click()
  await expect(form.getByText('All namespaces (*)')).toBeVisible()
  // Both "+ Add namespace" and "+ Wildcard (all)" are gone, and no individual row remains.
  await expect(form.getByRole('button', { name: /Add namespace/ })).toHaveCount(0)
  await expect(form.getByRole('button', { name: /Wildcard/ })).toHaveCount(0)
  await expect(form.getByLabel('namespace')).toHaveCount(0)
})

test('Bug: "Generate invite code" is disabled when there are zero grants', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  const form = page.getByRole('form', { name: 'Create invite' })
  const gen = page.getByRole('button', { name: 'Generate invite code' })

  // A valid email + a valid grant → enabled.
  await page.getByLabel('invitee email').fill('a@example.com')
  await form.getByLabel('namespace').first().fill('demo')
  await expect(gen).toBeEnabled()

  // Delete the only grant row → ZERO grants → disabled (the bug: it stayed enabled).
  await form.getByRole('button', { name: 'remove grant' }).first().click()
  await expect(form.getByLabel('namespace')).toHaveCount(0)
  await expect(gen).toBeDisabled()

  // Re-entering the email must NOT re-enable it while grants are zero.
  await page.getByLabel('invitee email').fill('')
  await page.getByLabel('invitee email').fill('b@example.com')
  await expect(gen).toBeDisabled()

  // All-rows-blank variant: add a row back but leave it blank → still disabled.
  await form.getByRole('button', { name: /Add namespace/ }).click()
  await expect(form.getByLabel('namespace')).toHaveCount(1)
  await expect(gen).toBeDisabled()
})

test('Bug 2: revoking the backing pending invite clears the displayed code', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  await page.getByLabel('invitee email').fill('bug2@example.com')
  await page.getByRole('form', { name: 'Create invite' }).getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  // The one-shot code box is shown (its <code> is the only one on the page).
  await expect(page.locator('code')).toHaveCount(1)
  // Revoke that pending invite → its code is now invalid and must clear from the screen.
  await page
    .getByRole('row', { name: /bug2@example.com/ })
    .getByRole('button', { name: 'Revoke' })
    .click()
  await expect(page.locator('code')).toHaveCount(0)
})

test('Improvement 3: an empty namespace is flagged invalid before submit', async ({ page }) => {
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  const form = page.getByRole('form', { name: 'Create invite' })
  const ns = form.getByLabel('namespace').first()
  // The default empty row is flagged BEFORE any submit, and submit is disabled.
  await expect(ns).toHaveAttribute('aria-invalid', 'true')
  await expect(page.getByRole('button', { name: 'Generate invite code' })).toBeDisabled()
  // Filling it clears the invalid state; with an email too, submit enables (the email
  // gate is exercised separately in "Bug A").
  await ns.fill('demo')
  await expect(ns).not.toHaveAttribute('aria-invalid', 'true')
  await page.getByLabel('invitee email').fill('a@example.com')
  await expect(page.getByRole('button', { name: 'Generate invite code' })).toBeEnabled()
})

test('Improvement 4: the copy button copies the invite code', async ({ page }) => {
  // Stub the clipboard before the app loads so the copy is observable and deterministic.
  await page.addInitScript(() => {
    ;(window as unknown as { __copied: string[] }).__copied = []
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: (t: string) => {
          ;(window as unknown as { __copied: string[] }).__copied.push(t)
          return Promise.resolve()
        },
      },
    })
  })
  await loginOrRegister(page, { accept: false })
  await openUserManagement(page)
  await page.getByLabel('invitee email').fill('copy@example.com')
  await page.getByRole('form', { name: 'Create invite' }).getByLabel('namespace').first().fill('demo')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()
  const copyBtn = page.getByRole('button', { name: 'Copy invite code' })
  await copyBtn.click()
  await expect(copyBtn).toHaveText('Copied')
  const copied = await page.evaluate(() => (window as unknown as { __copied: string[] }).__copied)
  expect(copied).toContain(code)
})

// B-28 password recovery case 1: an admin resets another user's password through the
// User-management UI; the user then signs in with the NEW password (the old one fails).
test('admin resets another user\'s password → user signs in with the new one', async ({ page }) => {
  const RESET_USER = 'resetme@example.com'
  const ORIG_PW = 'secondpw1234' // createSecondUser redeems with this password
  const NEW_PW = 'adminresetpw12'

  await loginOrRegister(page, { accept: false })
  await createSecondUser(page, RESET_USER) // leaves us in the admin session
  await openUserManagement(page)

  // Open the user's row and reset their password through the inline editor.
  const row = page.getByRole('row', { name: new RegExp(RESET_USER) })
  await row.getByRole('button', { name: 'Reset password' }).click()
  const editor = page.getByLabel('Reset password') // the editor container (aria-label)
  await editor.getByLabel('new password', { exact: true }).fill(NEW_PW)
  await editor.getByLabel('confirm new password').fill(NEW_PW)
  await editor.getByRole('button', { name: 'Reset password' }).click()
  // The editor closes on success (the row's action buttons return).
  await expect(row.getByRole('button', { name: 'Reset password' })).toBeVisible({ timeout: 15000 })

  // The user signs in with the NEW password.
  await page.context().clearCookies()
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible({ timeout: 15000 })
  await page.locator('#lg-email').fill(RESET_USER)
  await page.locator('#lg-pw').fill(NEW_PW)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeHidden({ timeout: 15000 })
  const ok = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(ok.authenticated).toBe(true)
  expect(ok.principal.email).toBe(RESET_USER)

  // The OLD password no longer works.
  await page.context().clearCookies()
  await page.goto(BASE)
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible({ timeout: 15000 })
  await page.locator('#lg-email').fill(RESET_USER)
  await page.locator('#lg-pw').fill(ORIG_PW)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Sign in to Shoka' })).toBeVisible()
  const fail = await page.evaluate(async () => (await fetch('/auth/status')).json())
  expect(fail.authenticated).toBe(false)
})
