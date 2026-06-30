import { test, expect } from '@playwright/test'

// Verify that form inputs in the SELF-ISSUED and CONFIDENTIAL CLIENTS sections
// are NOT cleared when a WebSocket "oauth.change" notification triggers a
// query refetch. The form state lives in local useState; this test confirms
// that the components stay mounted across the re-render.

test('self-issued and confidential form inputs preserved during query refetch', async ({ page }) => {
  await page.goto('/settings?item=oauth')
  await expect(page.getByRole('heading', { name: 'Trusted domains' })).toBeVisible()

  // Fill the self-issued form
  await page.getByTestId('self-issue-name').fill('my-test-token')
  await page.getByTestId('self-issue-scope').fill('ns:proj:rw')
  await page.getByTestId('self-issue-days').fill('14')

  // Fill the confidential form
  await page.getByTestId('client-issue-name').fill('my-connector')
  await page.getByTestId('client-issue-scope').fill('ns:other:r')
  await page.getByTestId('client-issue-validity').fill('60')

  // Trigger a real oauth.change notification by clicking Regenerate consent
  // on the seeded domain. Use dispatchEvent to bypass Playwright's
  // actionability checks (the "stable" wait hangs on this platform).
  await page.locator('[data-testid^="domain-consent-generate-"]').first()
    .dispatchEvent('click')

  // Wait for the NOTIFY round-trip: server mutation → notify center →
  // NOTIFY frame → notifyRouter → invalidateQueries → refetch → re-render
  await page.waitForTimeout(1500)

  // Verify all form values are preserved after the re-render
  await expect(page.getByTestId('self-issue-name')).toHaveValue('my-test-token')
  await expect(page.getByTestId('self-issue-scope')).toHaveValue('ns:proj:rw')
  await expect(page.getByTestId('self-issue-days')).toHaveValue('14')
  await expect(page.getByTestId('client-issue-name')).toHaveValue('my-connector')
  await expect(page.getByTestId('client-issue-scope')).toHaveValue('ns:other:r')
  await expect(page.getByTestId('client-issue-validity')).toHaveValue('60')
})
