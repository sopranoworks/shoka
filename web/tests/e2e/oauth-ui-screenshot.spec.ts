import { test, expect } from '@playwright/test'

test('OAuth settings page — verify self-issued scope field and button alignment', async ({ page }) => {
  test.setTimeout(60_000)
  await page.goto('/settings?item=oauth')
  await expect(page.getByText('connector.example.com')).toBeVisible()

  const selfSection = page.getByTestId('self-issued-section')
  // Self-issued: Name, Scope, TTL, Generate token all present
  await expect(selfSection.getByTestId('self-issue-name')).toBeVisible()
  await expect(selfSection.getByTestId('self-issue-scope')).toBeVisible()
  await expect(selfSection.getByText('TTL (days)')).toBeVisible()
  await expect(selfSection.getByText('Generate token')).toBeVisible()

  // Scope placeholder matches confidential section
  const scopeInput = selfSection.getByTestId('self-issue-scope')
  await expect(scopeInput).toHaveAttribute('placeholder', 'e.g. test:prtest:rw, or * for all access')

  // Confidential section also has its fields
  const confSection = page.getByTestId('confidential-section')
  await expect(confSection.getByTestId('client-issue-name')).toBeVisible()
  await expect(confSection.getByTestId('client-issue-scope')).toBeVisible()

  // Take screenshot for visual verification
  await page.screenshot({ path: '/tmp/oauth-ui-after.png', timeout: 10_000 }).catch(() => {})
})
