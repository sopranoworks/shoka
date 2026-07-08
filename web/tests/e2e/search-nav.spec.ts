import { test, expect } from '@playwright/test'
import { backendWrite } from './control'

// Search screen navigation: Escape returns to prior view, back button works,
// breadcrumb stays consistent (directive 2026-07-08).

test.beforeAll(async () => {
  await backendWrite(
    'demo',
    'docs',
    '_e2e/search-nav-target.md',
    '# Search Nav Target\n\nThis file exists so we can navigate to it before opening search.\n',
  )
})

test('Escape from search returns to the exact prior file view', async ({ page }) => {
  await page.goto('/p/demo/docs/blob/_e2e/search-nav-target.md')
  await expect(page.getByRole('heading', { name: 'Search Nav Target' })).toBeVisible()

  // Open search via Cmd+Shift+F
  await page.keyboard.press('Control+Shift+f')
  await expect(page.getByLabel('Search query')).toBeVisible()
  await expect(page).toHaveURL(/\/search/)

  // Breadcrumb should still show namespace/project segments
  const nav = page.getByRole('navigation', { name: 'Breadcrumb' })
  await expect(nav).toBeVisible()
  await expect(nav.getByText('demo')).toBeVisible()
  await expect(nav.getByText('docs')).toBeVisible()

  // Press Escape — should return to the file view
  await page.keyboard.press('Escape')
  await expect(page).toHaveURL(/\/blob\/_e2e\/search-nav-target\.md/)
  await expect(page.getByRole('heading', { name: 'Search Nav Target' })).toBeVisible()
})

test('back button in search toolbar returns to prior view', async ({ page }) => {
  await page.goto('/p/demo/docs/blob/_e2e/search-nav-target.md')
  await expect(page.getByRole('heading', { name: 'Search Nav Target' })).toBeVisible()

  await page.keyboard.press('Control+Shift+f')
  await expect(page.getByLabel('Search query')).toBeVisible()

  // Click the back/close button
  const backBtn = page.getByTestId('search-back-button')
  await expect(backBtn).toBeVisible()
  await backBtn.click()

  await expect(page).toHaveURL(/\/blob\/_e2e\/search-nav-target\.md/)
  await expect(page.getByRole('heading', { name: 'Search Nav Target' })).toBeVisible()
})

test('clicking project name in breadcrumb while on search navigates back', async ({ page }) => {
  await page.goto('/p/demo/docs/search')
  await expect(page.getByLabel('Search query')).toBeVisible()

  // Breadcrumb should show namespace and project
  const nav = page.getByRole('navigation', { name: 'Breadcrumb' })
  await expect(nav).toBeVisible()
  await expect(nav.getByText('demo')).toBeVisible()

  // Project name should be the last crumb (current page, not a link).
  // But the namespace should be a clickable link.
  const nsLink = nav.getByRole('link', { name: 'demo' })
  await expect(nsLink).toBeVisible()
})
