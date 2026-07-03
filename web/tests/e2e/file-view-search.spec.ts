import { test, expect } from '@playwright/test'
import { backendWrite } from './control'

// File-view UX improvements: copy-path button, in-view text search, and
// sidebar search results (directive 2026-07-01).

const SCREENSHOT_DIR = '/work/web/tests/e2e/screenshots'

test.beforeAll(async () => {
  await backendWrite(
    'demo',
    'docs',
    '_e2e/searchable-view.md',
    '# View Search Test\n\nThe quick brown fox jumps over the lazy dog.\n\n' +
      'Another paragraph with the word fox appearing twice: fox.\n\n' +
      'End of the test document.\n',
  )
})

// ---- Feature 1: Copy filename button ----

test('copy-path button shows checkmark feedback on click', async ({ page }) => {
  await page.goto('/p/demo/docs/blob/_e2e/searchable-view.md')
  await expect(page.getByRole('heading', { name: 'View Search Test' })).toBeVisible()

  const copyBtn = page.getByTestId('copy-path-button')
  await expect(copyBtn).toBeVisible()

  await copyBtn.click()
  await expect(copyBtn).toHaveAttribute('title', 'Copied!')

  await page.waitForTimeout(1600)
  await expect(copyBtn).toHaveAttribute('title', 'Copy file path')

  await page.screenshot({
    path: `${SCREENSHOT_DIR}/file-view-copy-button.png`,
    fullPage: true,
  })
})

// ---- Feature 2: In-view text search ----

test('in-view search: Ctrl+F opens search bar, finds matches, navigates', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/blob/_e2e/searchable-view.md')
  await expect(page.getByRole('heading', { name: 'View Search Test' })).toBeVisible()

  // Ctrl+F should open the in-view search bar (not browser find)
  await page.keyboard.press('Control+f')
  const searchBar = page.getByTestId('file-search-bar')
  await expect(searchBar).toBeVisible()

  // Type a search query — use the input inside the search bar
  const searchInput = searchBar.getByLabel('Find in file')
  await searchInput.fill('fox')

  // Should show match count ("1/3")
  const matchCount = page.getByTestId('search-match-count')
  await expect(matchCount).toBeVisible({ timeout: 3000 })
  await expect(matchCount).toContainText(/\/3$/)

  // Navigate forward with Enter — goes to "2/3"
  await searchInput.press('Enter')
  await expect(matchCount).toContainText(/^2\/3$/)

  // Navigate backward with Shift+Enter — back to "1/3"
  await searchInput.press('Shift+Enter')
  await expect(matchCount).toContainText(/^1\/3$/)

  await page.screenshot({
    path: `${SCREENSHOT_DIR}/file-view-search-active.png`,
    fullPage: true,
  })

  // Escape closes the search bar
  await searchInput.press('Escape')
  await expect(searchBar).not.toBeVisible()
})

test('in-view search: toggle button in toolbar opens/closes search', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/blob/_e2e/searchable-view.md')
  await expect(page.getByRole('heading', { name: 'View Search Test' })).toBeVisible()

  const toggleBtn = page.getByRole('button', { name: 'Toggle file search' })
  await toggleBtn.click()
  const searchBar = page.getByTestId('file-search-bar')
  await expect(searchBar).toBeVisible()

  // Click again to close
  await toggleBtn.click()
  await expect(searchBar).not.toBeVisible()
})

test('in-view search: case sensitivity toggle', async ({ page }) => {
  await page.goto('/p/demo/docs/blob/_e2e/searchable-view.md')
  await expect(page.getByRole('heading', { name: 'View Search Test' })).toBeVisible()

  await page.keyboard.press('Control+f')
  const searchBar = page.getByTestId('file-search-bar')
  const searchInput = searchBar.getByLabel('Find in file')
  await searchInput.fill('The')
  const matchCount = page.getByTestId('search-match-count')

  // Case-insensitive by default — should find "The" and "the"
  // Wait for matches to populate (async 80ms highlight pass)
  await expect(matchCount).toContainText('/', { timeout: 3000 })
  const text = await matchCount.textContent()
  const totalInsensitive = parseInt(text!.split('/')[1])

  // Toggle case-sensitive
  await searchBar.getByLabel('Match case').click()

  // Case-sensitive should find fewer (only "The" not "the") — poll until update lands
  await expect(async () => {
    const t = await matchCount.textContent()
    const n = parseInt(t!.split('/')[1])
    expect(n).toBeLessThan(totalInsensitive)
  }).toPass({ timeout: 3000 })
})

// ---- Feature 3: Search results in sidebar ----

test('sidebar search shows results inline and navigates to file', async ({
  page,
}) => {
  test.setTimeout(30_000)
  await page.goto('/p/demo/docs')
  await expect(page.getByRole('heading', { name: 'docs' })).toBeVisible()

  // Click the Search rail item to switch to search view
  await page.getByLabel('Search').click()

  // Type in the sidebar search
  const searchInput = page.getByLabel('Search files')
  await expect(searchInput).toBeVisible()
  await searchInput.fill('fox')

  // Results should appear in the sidebar (not the main pane)
  const results = page.getByTestId('sidebar-search-results')
  await expect(results).toBeVisible({ timeout: 5000 })
  await expect(results.locator('button').first()).toBeVisible()

  await page.screenshot({
    path: `${SCREENSHOT_DIR}/file-search-in-filetree.png`,
    fullPage: true,
  })

  // Click the first result
  await results.locator('button').first().click()

  // Should navigate to the blob view
  await expect(page).toHaveURL(/\/blob\//)
  // Wait for content + highlight pass to complete
  await expect(page.getByRole('heading', { name: 'View Search Test' })).toBeVisible()
  await page.waitForTimeout(200)

  await page.screenshot({
    path: `${SCREENSHOT_DIR}/file-search-select-to-view.png`,
    fullPage: true,
  })
})

test('sidebar search: clear button returns to hint text', async ({ page }) => {
  await page.goto('/p/demo/docs')
  await page.getByLabel('Search').click()

  const searchInput = page.getByLabel('Search files')
  await searchInput.fill('fox')

  // Results should appear
  const results = page.getByTestId('sidebar-search-results')
  await expect(results).toBeVisible({ timeout: 5000 })

  // Clear the search
  await page.getByLabel('Clear search').click()
  await expect(searchInput).toHaveValue('')
  await expect(results).not.toBeVisible()
})

test('sidebar search: content match auto-populates in-view search', async ({
  page,
}) => {
  test.setTimeout(30_000)
  await page.goto('/p/demo/docs')
  await page.getByLabel('Search').click()

  const searchInput = page.getByLabel('Search files')
  await searchInput.fill('fox')

  const results = page.getByTestId('sidebar-search-results')
  await expect(results).toBeVisible({ timeout: 5000 })

  // Click a result that has a snippet (content match)
  const firstResult = results.locator('button').first()
  await firstResult.click()

  // Should navigate to blob view with highlight param
  await expect(page).toHaveURL(/\/blob\//)

  // The in-view search bar should auto-open with the search term
  const fileSearchBar = page.getByTestId('file-search-bar')
  await expect(fileSearchBar).toBeVisible({ timeout: 5000 })
  await expect(fileSearchBar.getByLabel('Find in file')).toHaveValue('fox')
})
