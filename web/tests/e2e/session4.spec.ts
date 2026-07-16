import { test, expect, type Page } from '@playwright/test'
import { backendWrite } from './control'

// Session-4 features against the real binary + /ws/ui (see global-setup.ts):
// project-scoped search, markdown fence + code-file highlighting, the path-less
// new-file workflow, and scroll-position restoration. Tests that depend on file
// content seed their OWN file under _e2e/ via the control socket, so they never
// rely on shared fixtures that other specs mutate (reactive.spec rewrites
// README.md / backlog.md content).

const mainSave = (page: Page) =>
  page.getByRole('main').getByRole('button', { name: /Save/ })

// ---- Search ----

test('search finds a file by content and navigates to it', async ({ page }) => {
  // Seed a dedicated file with a unique content term, so the result is stable
  // regardless of what other specs did to the shared fixture.
  await backendWrite('demo', 'docs', '_e2e/searchable.md', '# Searchable\n\nA unique zebra marker.\n')

  await page.goto('/p/demo/docs')
  await expect(page.getByRole('heading', { name: 'docs' })).toBeVisible()
  await page.getByRole('link', { name: /Search/ }).click()
  await page.getByLabel('Search query').fill('zebra')
  await expect(page).toHaveURL(/\/search\?q=zebra$/)

  const result = page.getByRole('button', { name: /_e2e\/searchable\.md/ })
  await expect(result).toBeVisible()
  await result.click()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/_e2e/searchable.md$'))
  await expect(page.getByRole('heading', { name: 'Searchable' })).toBeVisible()
})

test('search reflects the typed query in the URL and shows results', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/search')
  await page.getByLabel('Search query').fill('Backlog')
  // The debounced input writes ?q= into the URL (deep-linkable state).
  await expect(page).toHaveURL(/\/search\?q=Backlog$/)
  await expect(page.getByRole('button', { name: /backlog\.md/ })).toBeVisible()
})

test('search shows a clear no-results state', async ({ page }) => {
  await page.goto('/p/demo/docs/search?q=zz-no-such-token-zz')
  await expect(page.getByText(/No results for/)).toBeVisible()
})

// ---- Syntax highlighting ----

test('markdown fenced code blocks are syntax-highlighted', async ({ page }) => {
  await page.goto('/p/demo/docs/blob/code.md')
  await expect(page.getByRole('heading', { name: 'Code' })).toBeVisible()
  // rehype-highlight tags the fenced <code> with the hljs class.
  await expect(page.locator('.hljs').first()).toBeVisible()
})

// (config.yaml CodeView highlighting is asserted in calibre.spec.ts.)

// ---- Path-less new file ----

test('new-file workflow: empty editor → type → Save with path → blob view', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/new')
  await expect(page.locator('.cm-editor')).toBeVisible()
  await page.locator('.cm-content').click()
  await page.keyboard.type('# Brand New\n\nfresh content\n')

  await mainSave(page).click()
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/brand-new.md')
  await dialog.getByRole('button', { name: 'Save' }).click()

  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/_e2e/brand-new.md$'))
  await expect(page.getByRole('heading', { name: 'Brand New' })).toBeVisible()
})

test('new file appears in sidebar without reload', async ({ page }) => {
  await page.goto('/p/demo/docs/new')
  await expect(page.locator('.cm-editor')).toBeVisible()
  await page.locator('.cm-content').click()
  await page.keyboard.type('# Sidebar Check\n\nThis file should appear in the tree.\n')

  await mainSave(page).click()
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/sidebar-check.md')
  await dialog.getByRole('button', { name: 'Save' }).click()

  await expect(page).toHaveURL(/\/p\/demo\/docs\/blob\/_e2e\/sidebar-check\.md$/)
  await expect(page.getByRole('heading', { name: 'Sidebar Check' })).toBeVisible()

  const sidebar = page.locator('#sidebar')
  await expect(sidebar.getByText('sidebar-check.md', { exact: true })).toBeVisible()
})

test('new-file path validation blocks a path with ".." and keeps the dialog open', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/new')
  await page.locator('.cm-content').click()
  await page.keyboard.type('content')

  await mainSave(page).click()
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('../escape.md')
  await dialog.getByRole('button', { name: 'Save' }).click()

  // The inline error mentions the offending segment, and we stay on /new.
  await expect(dialog.getByText(/\.\./)).toBeVisible()
  await expect(page).toHaveURL(/\/new$/)
})

test('new file in a project is reachable from the project-view header', async ({
  page,
}) => {
  await page.goto('/p/demo/docs')
  await page.getByRole('link', { name: 'New file' }).click()
  await expect(page).toHaveURL(/\/p\/demo\/docs\/new$/)
  await expect(page.locator('.cm-editor')).toBeVisible()
})

// ---- Scroll restoration ----

test('scroll position is restored on Back navigation (long file view)', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/blob/long.md')
  const body = page.locator('[data-scroll-restoration-id="file-body"]')
  await expect(body).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Long' })).toBeVisible()

  // Scroll down; setting scrollTop fires a scroll event the router captures.
  await body.evaluate((el) => {
    ;(el as HTMLElement).scrollTop = 800
  })
  await expect
    .poll(async () => body.evaluate((el) => (el as HTMLElement).scrollTop))
    .toBeGreaterThan(200)

  // Navigate to another file client-side, then Back. The first file's content is
  // cached (tall on return), so the router restores its scroll offset. (Reload
  // restoration is best-effort: the file content reloads async, so the element is
  // short when the auto-restore fires — see the honest finding in the report.)
  await page.getByRole('treeitem', { name: 'README.md' }).click()
  await expect(page.getByRole('heading', { name: 'Docs' })).toBeVisible()
  await page.goBack()
  await expect(page.getByRole('heading', { name: 'Long' })).toBeVisible()

  await expect
    .poll(
      async () =>
        page
          .locator('[data-scroll-restoration-id="file-body"]')
          .evaluate((el) => (el as HTMLElement).scrollTop),
      { timeout: 5000 },
    )
    .toBeGreaterThan(200)
})
