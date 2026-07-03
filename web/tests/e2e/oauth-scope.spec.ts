import { test, expect } from '@playwright/test'

test.describe('OAuth scope autocomplete and validation', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/settings?item=oauth')
    await expect(page.getByText('OAuth connections')).toBeVisible()
  })

  test('autocomplete: type namespace prefix, select namespace, project, then level', async ({ page }) => {
    const input = page.getByTestId('self-issue-scope')
    await input.click()
    await input.pressSequentially('d')

    const suggestions = page.getByTestId('self-issue-scope-suggestions')
    await expect(suggestions).toBeVisible()
    await expect(suggestions.getByRole('option').filter({ hasText: 'demo' })).toBeVisible()

    await page.screenshot({ path: 'test-results/oauth-scope-autocomplete.png', timeout: 10_000 }).catch(() => {})

    await suggestions.getByRole('option').filter({ hasText: 'demo' }).click()
    await expect(input).toHaveValue('demo:')

    await expect(suggestions.getByRole('option').filter({ hasText: 'docs' })).toBeVisible()

    await suggestions.getByRole('option').filter({ hasText: 'docs' }).click()
    await expect(input).toHaveValue('demo:docs:')

    await expect(suggestions.getByRole('option').filter({ hasText: 'rw' })).toBeVisible()

    await suggestions.getByRole('option').filter({ hasText: 'rw' }).click()
    await expect(input).toHaveValue('demo:docs:rw')
  })

  test('non-existent scope shows confirmation dialog; cancel returns to form', async ({ page }) => {
    await page.getByTestId('client-issue-scope').fill('fakens:fakeproj:rw')
    await page.getByTestId('client-issue-submit').click()

    const dialog = page.getByRole('dialog', { name: 'Non-existent scope references' })
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText("namespace 'fakens' not found")).toBeVisible()

    await page.screenshot({ path: 'test-results/oauth-scope-confirm-dialog.png', timeout: 10_000 }).catch(() => {})

    await dialog.getByText('Cancel').click()
    await expect(dialog).not.toBeVisible()
    await expect(page.getByTestId('client-issue-scope')).toHaveValue('fakens:fakeproj:rw')
  })

  test('confirm creates token despite non-existent scope', async ({ page }) => {
    await page.getByTestId('client-issue-scope').fill('staging:api:rw')
    await page.getByTestId('client-issue-submit').click()

    const dialog = page.getByRole('dialog', { name: 'Non-existent scope references' })
    await expect(dialog).toBeVisible()

    await page.getByTestId('scope-confirm-create').click()
    await expect(dialog).not.toBeVisible()
    await expect(page.getByTestId('client-issued-panel')).toBeVisible()
  })

  test('valid scope creates token without dialog', async ({ page }) => {
    await page.getByTestId('self-issue-scope').fill('demo:docs:rw')
    await page.getByRole('button', { name: 'Generate a token for the CLI' }).click()

    await expect(page.getByText('Copy this token now')).toBeVisible()

    await page.screenshot({ path: 'test-results/oauth-scope-valid-no-dialog.png', timeout: 10_000 }).catch(() => {})
  })
})
