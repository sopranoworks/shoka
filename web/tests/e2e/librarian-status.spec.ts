import { test, expect } from '@playwright/test'

// B-73 config-and-validation: the ask_the_librarian health surfaces as a
// super-user Settings item with a manual Refresh. The e2e server is configured
// with provider: "openai", model: "gpt-e2e-test", base_url: "http://localhost:1"
// — configured but unreachable, which is enough to test the full UI surface.

test('the Librarian item appears in the Settings list', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByText('Librarian')).toBeVisible()
})

test('Librarian status renders and Refresh re-checks (configured server)', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByRole('heading', { name: 'Librarian' })).toBeVisible()

  // Configured with an unreachable endpoint — status should settle to a
  // non-"unconfigured" value after a refresh.
  const refresh = page.getByTestId('librarian-refresh')
  await expect(refresh).toBeVisible()
  await refresh.click()

  // After refresh, the kind should be one of the failure kinds (not "unconfigured").
  await expect(page.getByTestId('librarian-kind')).not.toHaveText('Not configured')

  // The Reload-from-config action is present.
  const reload = page.getByTestId('librarian-reload')
  await expect(reload).toBeVisible()
})

test('Settings section is visible when librarian is configured', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByRole('heading', { name: 'Librarian' })).toBeVisible()
  await expect(page.getByTestId('librarian-settings')).toBeVisible()
})

test('Classifier section shows "Not configured" when classifier is disabled', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByRole('heading', { name: 'Librarian' })).toBeVisible()

  const section = page.getByTestId('classifier-section')
  await expect(section).toBeVisible()
  await expect(page.getByTestId('classifier-status')).toHaveText('Not configured')
})

test('Base URL field shows current value and saves a new value', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByTestId('librarian-settings')).toBeVisible()

  const input = page.getByTestId('base-url-input')
  await expect(input).toBeVisible()

  // The config sets base_url: "http://localhost:1" — verify it's shown.
  await expect(input).toHaveValue('http://localhost:1')

  // Save a new base URL.
  await input.fill('http://localhost:9999/v1')
  await page.getByTestId('base-url-save').click()

  // After save, the server recreates the client and health-checks. The status
  // card updates (the new URL is also unreachable, so the kind won't be "ready"
  // — we just verify the value round-tripped via the settings store).
  // Reload the page to prove persistence.
  await page.goto('/settings?item=librarian')
  await expect(page.getByTestId('librarian-settings')).toBeVisible()
  await expect(page.getByTestId('base-url-input')).toHaveValue('http://localhost:9999/v1')
})

test('max_steps persists across config reload', async ({ page }) => {
  await page.goto('/settings?item=librarian')
  await expect(page.getByTestId('librarian-settings')).toBeVisible()

  // Set max_steps to 15.
  const input = page.getByTestId('max-steps-input')
  await input.fill('15')
  await page.getByTestId('max-steps-save').click()
  await expect(input).toHaveValue('15')

  // Reload from config file — this re-reads the YAML (max_steps: 8) but the
  // persisted WebUI value (15) should win.
  await page.getByTestId('librarian-reload').click()

  // Wait for the reload to complete (status card updates).
  await expect(page.getByTestId('librarian-kind')).not.toHaveText('Not checked yet', { timeout: 10000 })

  // The max_steps value should still be 15 (persisted), not 8 (config default).
  await expect(input).toHaveValue('15')
})
