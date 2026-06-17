import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// B-28 — Namespace/project MANAGEMENT actions + user-management mutations, in a REAL
// browser against a REAL server over the REAL /ws/ui round-trip (the trash-D&D bar: a
// green render/mock test does NOT prove these work; only driving the real DOM + the real
// server mutation + the real NAMESPACE_HEALTH refetch does). Its own auth-enabled server
// (first-run admin), seeded with throwaway namespaces/projects BEFORE the admin exists
// (while /ws/ui is the no-lockout super-user pass-through). Covers ADD (namespace +
// project), RENAME (project + namespace; default shows none), MOVE (pick-target), DELETE
// (project type-confirm; namespace empty-only), permission-gated visibility (a
// namespace-admin via real /auth/status + admin-filtered NAMESPACE_HEALTH), and the
// user-management scope-edit + remove mutations. NO live data, NO live Chrome — a throwaway
// server + throwaway namespaces/projects/accounts the Coder ran headless.

const PORT = 8094
const MCP_PORT = 8093
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
  throw new Error(`nsproj-manage server did not become ready at ${url}`)
}

// Seed throwaway namespaces/projects over /ws/ui while the user store is EMPTY (super-user
// pass-through), before the first admin is created.
async function seed(): Promise<void> {
  const rpc = (ws: WebSocket, type: string, payload: unknown) =>
    new Promise<void>((resolve, reject) => {
      const onMsg = (ev: MessageEvent) => {
        const m = JSON.parse(String(ev.data)) as { type: string }
        if (m.type === 'NOTIFY') return
        ws.removeEventListener('message', onMsg)
        m.type === 'ERROR' ? reject(new Error('seed error: ' + String(ev.data))) : resolve()
      }
      ws.addEventListener('message', onMsg)
      ws.send(JSON.stringify({ type, payload }))
    })
  const ws = new WebSocket(`ws://localhost:${PORT}/ws/ui`)
  await new Promise<void>((resolve, reject) => {
    ws.addEventListener('open', () => resolve())
    ws.addEventListener('error', () => reject(new Error('seed ws connect failed')))
  })
  // Projects (each in its own namespace so the mutating tests don't interfere).
  const proj = (ns: string, name: string) => rpc(ws, 'CREATE_PROJECT', { namespace: ns, projectName: name })
  await proj('addns', 'existing') // add-project target + non-empty for the delete-disabled assertion
  await proj('rnp', 'old') // rename-project subject
  await proj('rnn', 'keep') // rename-namespace subject (non-empty: rename is a relabel, allowed)
  await proj('mvs', 'mover') // move source
  await proj('delp', 'victim') // delete-project subject
  await proj('permns', 'pp') // permission-gating: the namespace-admin's own namespace
  // Empty namespaces (CREATE_NAMESPACE).
  await rpc(ws, 'CREATE_NAMESPACE', { namespace: 'mvd' }) // move target (must pre-exist)
  await rpc(ws, 'CREATE_NAMESPACE', { namespace: 'delempty' }) // delete-namespace empty-only subject
  ws.close()
}

test.beforeAll(async () => {
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-nsproj-'))
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

// Ensure a session: the FIRST test sees the first-run wizard and creates the admin; later
// tests see the login screen. Branch on whichever appears (mirrors settings.spec.ts).
async function loginAdmin(page: Page) {
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

async function openNsProj(page: Page) {
  await page.goto(`${BASE}/settings?item=namespaces`)
  await expect(page.getByRole('heading', { name: 'Namespace / project management' })).toBeVisible()
}

// ADD project (real CREATE_PROJECT + NAMESPACE_HEALTH refetch).
test('add project: creating a project shows it in the listing', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  const block = page.getByTestId('ns-addns')
  await block.getByRole('button', { name: '+ Add project' }).click()
  await page.getByLabel('Project name').fill('fresh')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByTestId('proj-addns-fresh')).toBeVisible()
})

// ADD namespace (real CREATE_NAMESPACE).
test('add namespace: creating a namespace shows a new block', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  await page.getByRole('button', { name: '+ New namespace' }).click()
  await page.getByLabel('Namespace name').fill('createdns')
  await page.getByRole('button', { name: 'Create' }).click()
  await expect(page.getByTestId('ns-createdns')).toBeVisible()
})

// RENAME project (real RENAME_PROJECT) — the edit-the-name dialog, distinct from move/delete.
test('rename project: the new name replaces the old in the listing', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  const row = page.getByTestId('proj-rnp-old')
  await expect(row).toBeVisible()
  await within(page, 'rename-slot-rnp-old').getByRole('button', { name: 'Rename…' }).click()
  const dialog = page.getByRole('dialog', { name: /rename project old/i })
  // It is the edit-the-name dialog — NOT a pick-target select, NOT a type-to-destroy input.
  await expect(dialog.getByLabel('target namespace')).toHaveCount(0)
  await expect(dialog.getByLabel('confirm name')).toHaveCount(0)
  const input = dialog.getByLabel('New project name')
  await input.fill('renamed')
  await dialog.getByRole('button', { name: 'Rename' }).click()
  await expect(page.getByTestId('proj-rnp-renamed')).toBeVisible()
  await expect(page.getByTestId('proj-rnp-old')).toHaveCount(0)
})

// RENAME namespace (real RENAME_NAMESPACE) + default shows NO rename affordance.
test('rename namespace: relabels the block; default has no rename', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  // The default namespace (if listed) shows no rename affordance.
  await expect(page.getByTestId('ns-rename-default')).toHaveCount(0)
  await page.getByTestId('ns-rename-rnn').click()
  const dialog = page.getByRole('dialog', { name: /rename namespace rnn/i })
  await dialog.getByLabel('New namespace name').fill('rnn2')
  await dialog.getByRole('button', { name: 'Rename' }).click()
  await expect(page.getByTestId('ns-rnn2')).toBeVisible()
  await expect(page.getByTestId('ns-rnn')).toHaveCount(0)
  // The project travelled with the relabel.
  await expect(page.getByTestId('proj-rnn2-keep')).toBeVisible()
})

// MOVE project (real MOVE_PROJECT) — pick-target dialog, distinct from delete.
test('move project: relocates to the chosen namespace', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  await within(page, 'move-slot-mvs-mover').getByRole('button', { name: 'Move…' }).click()
  const dialog = page.getByRole('dialog', { name: /move project mover/i })
  await dialog.getByLabel('target namespace').selectOption('mvd')
  await dialog.getByRole('button', { name: 'Move' }).click()
  await expect(page.getByTestId('proj-mvd-mover')).toBeVisible()
  await expect(page.getByTestId('proj-mvs-mover')).toHaveCount(0)
})

// DELETE project (real DELETE_PROJECT) — type-the-exact-name confirm.
test('delete project: type-confirm actually removes the project', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  const row = page.getByTestId('proj-delp-victim')
  await expect(row).toBeVisible()
  await row.getByRole('button', { name: 'Delete' }).click()
  const dialog = page.getByRole('dialog')
  const confirm = dialog.getByRole('button', { name: 'Delete' })
  await expect(confirm).toBeDisabled()
  await dialog.getByLabel('confirm name').fill('victim')
  await expect(confirm).toBeEnabled()
  await confirm.click()
  await expect(page.getByTestId('proj-delp-victim')).toHaveCount(0)
})

// DELETE namespace (real DELETE_NAMESPACE) — empty-only: disabled while non-empty, then
// type-confirm removes the empty one.
test('delete namespace: empty-only, type-confirmed', async ({ page }) => {
  await loginAdmin(page)
  await openNsProj(page)
  // A non-empty namespace's Delete is disabled.
  const nonEmpty = page.getByTestId('ns-addns').getByRole('button', { name: 'Delete namespace' })
  await expect(nonEmpty).toBeDisabled()
  // The empty namespace deletes after type-confirm.
  await page.getByTestId('ns-delempty').getByRole('button', { name: 'Delete namespace' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('confirm name').fill('delempty')
  await dialog.getByRole('button', { name: 'Delete' }).click()
  await expect(page.getByTestId('ns-delempty')).toHaveCount(0)
})

// Permission-gated visibility (real /auth/status + admin-filtered NAMESPACE_HEALTH): a
// namespace-admin (admin on permns only) sees its own namespace's project controls
// (Rename…/Delete/Add project) but NOT the super-user-only ones (+ New namespace, the
// namespace-level Rename…/Delete namespace), and does NOT see namespaces it cannot administer.
test('permission gating: a namespace-admin sees only its namespace and no super-user controls', async ({ page }) => {
  await loginAdmin(page)
  // As admin, mint an invite scoped to permns:admin.
  await page.goto(`${BASE}/settings?item=users`)
  await page.getByLabel('invitee email').fill('nsadmin@example.com')
  await page.getByLabel('namespace').first().fill('permns')
  await page.getByLabel('level').first().selectOption('admin')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()

  // Redeem as the scoped invitee.
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.locator('#rd-pw').fill('nsadminpw123')
  await page.locator('#rd-pw2').fill('nsadminpw123')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })

  await openNsProj(page)
  // Its own namespace is listed with project-level controls.
  const own = page.getByTestId('ns-permns')
  await expect(own).toBeVisible()
  await expect(own.getByRole('button', { name: '+ Add project' })).toBeVisible()
  await expect(within(page, 'rename-slot-permns-pp').getByRole('button', { name: 'Rename…' })).toBeVisible()
  await expect(page.getByTestId('proj-permns-pp').getByRole('button', { name: 'Delete' })).toBeVisible()
  // NO super-user-only controls.
  await expect(page.getByRole('button', { name: '+ New namespace' })).toHaveCount(0)
  await expect(page.getByTestId('ns-rename-permns')).toHaveCount(0)
  await expect(own.getByRole('button', { name: 'Delete namespace' })).toHaveCount(0)
  // Move is super-user only — no move control for a namespace-admin.
  await expect(within(page, 'move-slot-permns-pp').getByRole('button', { name: 'Move…' })).toHaveCount(0)
  // Namespaces it cannot administer are NOT listed (admin-filtered NAMESPACE_HEALTH).
  await expect(page.getByTestId('ns-addns')).toHaveCount(0)
  await expect(page.getByTestId('ns-mvs')).toHaveCount(0)
})

// USER MANAGEMENT — scope edit (real ADMIN_SET_USER_SCOPE): change a user's permissions and
// confirm the rendered scope description reflects it after the round-trip.
test('user management: edit a user scope and it persists', async ({ page }) => {
  await loginAdmin(page)
  // Ensure there is a non-self user: redeem one via an invite first.
  await page.goto(`${BASE}/settings?item=users`)
  await page.getByLabel('invitee email').fill('scoped@example.com')
  await page.getByLabel('namespace').first().fill('rnp')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.locator('#rd-pw').fill('scopedpw1234')
  await page.locator('#rd-pw2').fill('scopedpw1234')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })

  // Back as admin: edit scoped@'s permissions.
  await page.context().clearCookies()
  await loginAdmin(page)
  await page.goto(`${BASE}/settings?item=users`)
  // Scope to the Users table (the first table) — the pending-invites table also names the user.
  const usersTable = page.getByRole('table').first()
  const row = usersTable.getByRole('row', { name: /scoped@example\.com/ })
  await expect(row).toBeVisible()
  await row.getByRole('button', { name: 'Edit permissions' }).click()
  const editor = page.getByLabel('Scope editor')
  await editor.getByLabel('level').first().selectOption('admin')
  await editor.getByRole('button', { name: 'Save' }).click()
  // The round-trip persisted: re-render shows the admin level for that user's namespace.
  await expect(usersTable.getByRole('row', { name: /scoped@example\.com/ })).toContainText(/admin/i)
})

// USER MANAGEMENT — remove (real ADMIN_REMOVE_USER): removing a user drops the row.
test('user management: remove a user drops the row', async ({ page }) => {
  await loginAdmin(page)
  // Invite + redeem a disposable user, then remove it as admin.
  await page.goto(`${BASE}/settings?item=users`)
  await page.getByLabel('invitee email').fill('removeme@example.com')
  await page.getByLabel('namespace').first().fill('rnp')
  await page.getByRole('button', { name: 'Generate invite code' }).click()
  const code = await page.locator('code').first().innerText()
  await page.context().clearCookies()
  await page.goto(BASE)
  await page.getByRole('button', { name: 'Have an invite code?' }).click()
  await page.locator('#rd-code').fill(code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.locator('#rd-pw').fill('removemepw12')
  await page.locator('#rd-pw2').fill('removemepw12')
  await page.getByRole('button', { name: 'Create account' }).click()
  await expect(page.getByRole('heading', { name: 'Set up your account' })).toBeHidden({ timeout: 15000 })

  await page.context().clearCookies()
  await loginAdmin(page)
  await page.goto(`${BASE}/settings?item=users`)
  // Scope to the Users table — the pending-invites table also names the user.
  const usersTable = page.getByRole('table').first()
  const row = usersTable.getByRole('row', { name: /removeme@example\.com/ })
  await expect(row).toBeVisible()
  // Only NOW arm the dialog handler — the Remove action uses window.confirm. (Registering it
  // earlier interferes with the redeem navigation above.)
  page.once('dialog', (d) => d.accept())
  await row.getByRole('button', { name: 'Remove' }).click()
  await expect(usersTable.getByRole('row', { name: /removeme@example\.com/ })).toHaveCount(0)
})

// Scope a getByTestId lookup to find controls within a row's named slot.
function within(page: Page, testId: string) {
  return page.getByTestId(testId)
}
