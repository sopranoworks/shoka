import { test, expect, type Page } from '@playwright/test'
import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, existsSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// B-28 — the orphaned-data rendering in the Namespace/project management view, in a REAL
// browser reached THROUGH THE NORMAL UI PATH (gear → "Namespace / project management" — NOT
// page.goto to a deep route). Proves the .deleted.db sibling-blindness fix end-to-end on the
// rendered DOM: a genuine stray `default/shoka.db` (no project dir) renders an orphaned row
// (full filename + Clean) under the DEFAULT block; a LIVE `shoka/maintenance` with a
// `maintenance.deleted.db` shows NO orphaned row (the classifier maps it to the live project);
// "shoka" appears only under default (no front-end mis-attribution); a live sibling is never
// offered Clean. RED on reverting the dbBaseName `.deleted.db` branch. Throwaway server +
// throwaway data — never live data, never live Chrome.

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..')
const PORT = 8096
const MCP_PORT = 8095
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
  throw new Error(`orphaned-render server did not become ready at ${url}`)
}

// Seed over /ws/ui while the user store is EMPTY (super-user pass-through): create a live
// shoka/maintenance project, then write+delete a file so a real maintenance.deleted.db is
// produced (created by the op:"delete" commit-land hook).
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
  await rpc(ws, 'CREATE_PROJECT', { namespace: 'shoka', projectName: 'maintenance' })
  await rpc(ws, 'SAVE_FILE', { namespace: 'shoka', projectName: 'maintenance', path: 'doomed.md', content: '# doomed\n' })
  await rpc(ws, 'DELETE_FILE', { namespace: 'shoka', projectName: 'maintenance', path: 'doomed.md' })
  ws.close()
}

test.beforeAll(async () => {
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-orphaned-'))
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

  const base = join(dataDir, 'data')
  // (a) A genuine stray catalog with NO project dir, in the always-managed `default`
  // namespace: default/@shoka.project.db (named "shoka" — the field collision that confused
  // the operator). Uses the @<project>.<kind>.db pattern (kind=project).
  mkdirSync(join(base, 'default'), { recursive: true })
  writeFileSync(join(base, 'default', '@shoka.project.db'), '')

  // (b) Wait for the op:"delete" commit-land hook to have produced @maintenance.deleted.db
  // (it lands after the async WAL commit, post DELETE_ACK).
  const deletedDb = join(base, 'shoka', '@maintenance.deleted.db')
  const deadline = Date.now() + 15000
  while (!existsSync(deletedDb)) {
    if (Date.now() > deadline) throw new Error('@maintenance.deleted.db was not created by the delete')
    await new Promise((r) => setTimeout(r, 150))
  }
})

test.afterAll(() => {
  server?.kill('SIGTERM')
  if (dataDir) rmSync(dataDir, { recursive: true, force: true })
})

async function loginAdmin(page: Page) {
  page.on('dialog', (d) => d.dismiss()) // dismiss the first-run passkey offer (password-only)
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

// Reach the management view the way a user does: open a project → gear/Settings → the
// "Namespace / project management" item. NOT page.goto to a deep route.
async function openManagementViaUI(page: Page) {
  await page.goto(`${BASE}/p/shoka/maintenance`)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('link', { name: 'Namespace / project management' }).click()
  await expect(page.getByRole('heading', { name: 'Namespace / project management' })).toBeVisible()
}

test('orphaned rendering: genuine stray shows under default with full filename; a live .deleted.db is not flagged; no cross-attribution', async ({ page }) => {
  await loginAdmin(page)
  await openManagementViaUI(page)

  // Both blocks render.
  await expect(page.getByTestId('ns-default')).toBeVisible()
  await expect(page.getByTestId('ns-shoka')).toBeVisible()

  // DEFAULT block: the genuine stray default/@shoka.project.db is an orphaned row showing the
  // FULL filename, and a Clean control IS offered (it is not a live project's sibling).
  const defOrphan = page.getByTestId('orphan-default-shoka')
  await expect(defOrphan).toBeVisible()
  await expect(defOrphan).toContainText('@shoka.project.db')
  await expect(defOrphan.getByRole('button', { name: 'Clean' })).toBeVisible()

  // SHOKA block: the live maintenance project's maintenance.deleted.db is NOT flagged orphaned
  // (the classifier maps it to base "maintenance", a live project) — so the shoka block has NO
  // orphaned section at all, and specifically no "maintenance.deleted" row.
  await expect(page.getByTestId('orphaned-shoka')).toHaveCount(0)
  await expect(page.getByTestId('orphan-shoka-maintenance.deleted')).toHaveCount(0)

  // No cross-namespace front-end mis-attribution: "shoka" appears ONLY under the default block.
  await expect(page.getByTestId('orphan-shoka-shoka')).toHaveCount(0)
})
