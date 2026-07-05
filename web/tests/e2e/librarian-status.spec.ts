import { test, expect } from '@playwright/test'

// B-73 config-and-validation: the ask_the_librarian health surfaces as a
// super-user Settings item with a manual Refresh. The e2e server has no `llm`
// block configured, so the status is deterministically "Not configured" — which
// is exactly what proves the surface is wired end to end (registry item →
// SettingsPage route → LIBRARIAN_STATUS over /ws/ui → rendered status).

test('the Librarian item appears in the Settings list', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByText('Librarian')).toBeVisible()
})

test('Librarian status renders and Refresh re-checks (unconfigured server)', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByRole('heading', { name: 'Librarian' })).toBeVisible()

  // No llm configured on the e2e server ⇒ "Not configured".
  await expect(page.getByTestId('librarian-kind')).toHaveText('Not configured')

  const refresh = page.getByTestId('librarian-refresh')
  await expect(refresh).toBeVisible()
  await refresh.click()

  // A manual refresh re-runs the server check; with no LLM it stays unconfigured.
  await expect(page.getByTestId('librarian-kind')).toHaveText('Not configured')

  // The Reload-from-config action is present; on a server with no llm block it
  // reports unconfigured (a reload cannot enable a never-registered librarian).
  const reload = page.getByTestId('librarian-reload')
  await expect(reload).toBeVisible()
  await reload.click()
  await expect(page.getByTestId('librarian-kind')).toHaveText('Not configured')
})

test('Classifier section shows "Not configured" when classifier is disabled', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByRole('heading', { name: 'Librarian' })).toBeVisible()

  // The classifier section should be present and show "Not configured".
  const section = page.getByTestId('classifier-section')
  await expect(section).toBeVisible()
  await expect(page.getByTestId('classifier-status')).toHaveText('Not configured')
})
