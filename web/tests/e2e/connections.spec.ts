import { test, expect } from '@playwright/test'

// The OAuth connection management view (B-39 (c)) against the real binary. The
// e2e harness (global-setup.ts) enables OAuth and seeds ONE token series into
// the store before the server starts, so the view has a real connection to list
// and revoke. The connecting client is shown by its CIMD domain placeholder
// (connector.example.com — §0(b)); no secret is ever shown or sent.
//
// In single-user mode the operator is the administrator, so the admin-gated
// surface is visible and usable. Tests run serially (workers: 1); the revoke
// test runs LAST because it removes the single seeded connection.

test('lists the seeded connection by client domain + principal, with no secret on screen or in frames', async ({
  page,
}) => {
  // Capture every inbound ws frame so we can assert no secret crosses the wire.
  const inbound: string[] = []
  page.on('websocket', (ws) => {
    ws.on('framereceived', (f) => {
      if (typeof f.payload === 'string') inbound.push(f.payload)
    })
  })

  await page.goto('/admin/connections')

  // The seeded connection: shown by its CIMD domain and bound principal.
  await expect(page.getByText('connector.example.com')).toBeVisible()
  await expect(page.getByText('Op Erator')).toBeVisible()
  await expect(page.getByText('op@example.test')).toBeVisible()

  // No secret on screen: no token text anywhere in the rendered page.
  const body = (await page.locator('body').textContent()) ?? ''
  expect(body).not.toMatch(/token/i)

  // No secret in the wire frames either: the OAUTH_LIST response arrived, and no
  // frame carries an access/refresh token field or value.
  const listFrame = inbound.find((f) => f.includes('OAUTH_LIST'))
  expect(listFrame, 'OAUTH_LIST response frame was received').toBeTruthy()
  for (const f of inbound) {
    expect(f).not.toContain('access_token')
    expect(f).not.toContain('refresh_token')
  }
})

test('the admin palette entry opens the management view', async ({ page }) => {
  await page.goto('/')
  await page.keyboard.press('Meta+k')
  await expect(page.getByPlaceholder('Type a command or search…')).toBeVisible()
  await page.getByText('Manage OAuth connections…').click()
  await expect(page).toHaveURL(/\/admin\/connections$/)
  await expect(page.getByText('connector.example.com')).toBeVisible()
})

test('per-row revoke is inline-confirm gated, and the row drops on confirm', async ({
  page,
}) => {
  await page.goto('/admin/connections')
  // Scope to the table cell so the assertion is not satisfied by the revoke
  // success toast (which also names the client domain).
  const row = page.getByRole('cell', { name: 'connector.example.com' })
  await expect(row).toBeVisible()

  // First click only arms the confirm — destructive actions are gated.
  await page.getByRole('button', { name: 'Revoke' }).click()
  const confirm = page.getByRole('button', { name: 'Confirm revoke' })
  await expect(confirm).toBeVisible()

  // Confirm cuts exactly that connection; the row drops and the empty state shows.
  await confirm.click()
  await expect(row).toBeHidden()
  await expect(page.getByText('No active OAuth connections.')).toBeVisible()
})
