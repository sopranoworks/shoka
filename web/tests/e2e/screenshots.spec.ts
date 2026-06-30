import { test, expect } from '@playwright/test'

const pages = [
  { name: 'oauth', path: '/settings?item=oauth', waitFor: 'Trusted domains' },
  { name: 'server-info', path: '/settings?item=server-info', waitFor: 'Server' },
]

for (const p of pages) {
  test(`screenshot: ${p.name}`, async ({ page }) => {
    test.setTimeout(60_000)
    await page.goto(p.path)
    await expect(page.getByRole('heading', { name: p.waitFor })).toBeVisible()
    await page.screenshot({
      path: `/work/web/tests/e2e/screenshots/${p.name}.png`,
      fullPage: true,
    })
  })
}
