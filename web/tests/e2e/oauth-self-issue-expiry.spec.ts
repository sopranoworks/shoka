import { test, expect } from '@playwright/test'

// B-71 Stage 4 (final) — the real-browser, through-UI proof that the operator's per-issuance
// FINITE expiry choice is honored on the self/operator-issued ("token to self") path, and that
// the picker offers no indefinite option. Super-user → settings → choose a finite expiry →
// Generate. The issued token's lifetime must reflect the choice, not the 1h global default.

interface IssuedSelf {
  access_token: string
  access_expiry: string
}

test('self-issue a CLI token with a chosen finite expiry; the lifetime reflects the choice', async ({
  page,
}) => {
  // Capture the OAUTH_ISSUE_SELF response frame to read the minted token's exact expiry.
  const issued: IssuedSelf[] = []
  page.on('websocket', (ws) => {
    ws.on('framereceived', (f) => {
      if (typeof f.payload !== 'string' || !f.payload.includes('OAUTH_ISSUE_SELF')) return
      try {
        const m = JSON.parse(f.payload) as { type: string; payload?: IssuedSelf }
        if (m.type === 'OAUTH_ISSUE_SELF' && m.payload?.access_expiry) issued.push(m.payload)
      } catch {
        // not the frame we want
      }
    })
  })

  await page.goto('/settings?item=oauth')
  // Choose a finite 7-day expiry (GitHub-PAT-style; no indefinite option).
  await page.getByTestId('self-issue-days').fill('7')
  const t0 = Date.now()
  await page.getByRole('button', { name: 'Generate a token for the CLI' }).click()

  // The display-once panel appears…
  await expect(page.getByText('Copy this token now.')).toBeVisible()
  // …and the minted token's lifetime reflects the chosen 7 days — NOT the 1h global default.
  await expect.poll(() => issued.length).toBeGreaterThan(0)
  const expiryMs = new Date(issued[issued.length - 1].access_expiry).getTime()
  const days = (expiryMs - t0) / 86_400_000
  expect(days, `chosen 7-day expiry must be honored, got ~${days.toFixed(3)}d`).toBeGreaterThan(2)
  expect(days, 'and must be ~7 days, not unbounded/huge').toBeLessThan(14)
})

test('the self-issue picker offers no indefinite option (invalid value disables Generate)', async ({
  page,
}) => {
  await page.goto('/settings?item=oauth')
  const gen = page.getByRole('button', { name: 'Generate a token for the CLI' })
  // 0 / negative are not valid finite positive choices — Generate is disabled, no "never" option.
  await page.getByTestId('self-issue-days').fill('0')
  await expect(gen).toBeDisabled()
  await page.getByTestId('self-issue-days').fill('-5')
  await expect(gen).toBeDisabled()
  await page.getByTestId('self-issue-days').fill('7')
  await expect(gen).toBeEnabled()
})
