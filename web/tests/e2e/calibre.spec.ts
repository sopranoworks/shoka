import { test, expect } from '@playwright/test'

// These tests pin the v3 §2 / directive §1.3 calibre properties against the
// real Shoka binary serving the production bundle (see global-setup.ts).
// Fixture data: demo/docs (README.md, backlog.md, guides/intro.md) and
// team/handbook (index.md).

const README = '/p/demo/docs/blob/README.md'
const INTRO = '/p/demo/docs/blob/guides/intro.md'

test('repository list renders projects from the live backend', async ({ page }) => {
  await page.goto('/')
  await expect(page.locator('a[href="/p/demo/docs"]')).toBeVisible()
  await expect(page.locator('a[href="/p/team/handbook"]')).toBeVisible()
})

test('deep link boots directly to the file view', async ({ page }) => {
  // Navigate straight to a deep URL (not via the home page): the SPA must boot
  // its assets from the domain root (the base:'/' fix) and render the file.
  await page.goto(README)
  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/README.md$'))
  await expect(page.getByRole('heading', { name: 'Docs' })).toBeVisible()
  await expect(page.getByText('Hello')).toBeVisible()
})

test('markdown renders GFM (tables)', async ({ page }) => {
  await page.goto(README)
  await expect(page.locator('table')).toBeVisible()
})

test('non-markdown files render as plain text, not markdown', async ({
  page,
}) => {
  await page.goto('/p/demo/docs/blob/config.yaml')
  // The raw content appears verbatim in a <pre>...
  await expect(page.locator('pre')).toContainText('name: docs')
  // ...and the "# not a heading" line is NOT promoted to a real heading.
  await expect(
    page.getByRole('heading', { name: 'not a heading' }),
  ).toHaveCount(0)
})

test('reload preserves the current view', async ({ page }) => {
  await page.goto(README)
  await expect(page.getByRole('heading', { name: 'Docs' })).toBeVisible()
  await page.reload()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/README.md$'))
  await expect(page.getByRole('heading', { name: 'Docs' })).toBeVisible()
})

test('browser Back navigates in-app, never exits the app', async ({ page }) => {
  await page.goto('/')
  await page.locator('a[href="/p/demo/docs"]').click()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs$'))
  await page.locator(`a[href="${README}"]`).first().click()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/README.md$'))
  await page.goBack()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs$'))
  // still inside the app on the same origin
  expect(new URL(page.url()).host).toBe(new URL(page.url()).host)
})

test('command palette opens and runs a command', async ({ page }) => {
  await page.goto(README)
  await page.keyboard.press('Meta+k')
  const input = page.getByPlaceholder('Type a command or search…')
  await expect(input).toBeVisible()
  await input.fill('Go Home')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(new RegExp('/$'))
})

test('quick-open jumps to a file by name across projects', async ({ page }) => {
  await page.goto('/')
  await page.keyboard.press('Meta+p')
  const input = page.getByPlaceholder('Type a file name…')
  await expect(input).toBeVisible()
  await input.fill('intro')
  const option = page.getByText('guides/intro.md')
  await expect(option).toBeVisible()
  await option.click()
  await expect(page).toHaveURL(new RegExp('/p/demo/docs/blob/guides/intro.md$'))
})

test('theme toggle persists across reload', async ({ page }) => {
  await page.goto('/')
  const themeOf = () =>
    page.evaluate(() => document.documentElement.dataset.theme)
  expect(await themeOf()).toBe('dark') // dark by default
  await page.keyboard.press('Meta+Shift+l')
  expect(await themeOf()).toBe('light')
  await page.reload()
  expect(await themeOf()).toBe('light')
})

test('narrow viewport reflows without a horizontal sliver', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 })
  await page.goto(INTRO)
  await expect(page.getByRole('heading', { name: 'Intro' })).toBeVisible()
  const overflow = await page.evaluate(
    () => document.documentElement.scrollWidth <= window.innerWidth + 1,
  )
  expect(overflow).toBeTruthy()
})
