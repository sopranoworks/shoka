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
  await expect(elements.getByText('HTTP')).toBeVisible()
})
