import { test, expect } from '@playwright/test'

// The Server Info settings page displays the server's network endpoints
// (HTTP + MCP listeners). The e2e server always has at least an HTTP
// listener, so the page renders deterministically.

test('Server Info item appears in the Settings list', async ({ page }) => {
  await page.goto('/settings')
  await expect(page.getByText('Server Info')).toBeVisible()
})

test('Server Info page renders network elements', async ({ page }) => {
  await page.goto('/settings?item=server-info')
  await expect(page.getByRole('heading', { name: 'Server Info' })).toBeVisible()

  // The e2e server always has an HTTP listener.
  const elements = page.getByTestId('server-info-elements')
  await expect(elements).toBeVisible()
  await expect(elements.getByText('HTTP', { exact: true })).toBeVisible()
})

test('Copy button copies the external URL to clipboard', async ({ page }) => {
  await page.addInitScript(() => {
    ;(window as unknown as { __copied: string[] }).__copied = []
    Clipboard.prototype.writeText = function (t: string) {
      ;(window as unknown as { __copied: string[] }).__copied.push(t)
      return Promise.resolve()
    }
  })
  await page.goto('/settings?item=server-info')
  const elements = page.getByTestId('server-info-elements')
  await expect(elements).toBeVisible()

  const copyBtn = elements.getByTestId('copy-url-button').first()
  await expect(copyBtn).toBeVisible()
  await copyBtn.click()
  await expect(copyBtn).toHaveText('Copied')

  const copied = await page.evaluate(() => (window as unknown as { __copied: string[] }).__copied)
  expect(copied.length).toBeGreaterThan(0)
  expect(copied[0]).toContain('localhost')
})
