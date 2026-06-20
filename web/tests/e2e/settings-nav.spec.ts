import { test, expect } from '@playwright/test'

// The settings-nav-stays-active bugfix (2026-06-20): after opening Settings, clicking "Shoka" in
// the top bar returns to the projects list, but the Settings (gear) rail item used to STAY visually
// active — the rail's active state drifted from the actual view. The fix derives the rail's
// "settings" active state from the current route (single source of truth), so it clears the moment
// "Shoka"/home navigates away. Driven through the real UI (clicks, not page.goto).

test('the Settings gear clears its active state when returning to projects via "Shoka"', async ({
  page,
}) => {
  await page.goto('/')
  const gear = page.getByRole('button', { name: 'Settings' })
  await expect(gear).toBeVisible()
  // Not active on the projects list.
  await expect(gear).toHaveAttribute('aria-pressed', 'false')

  // Open Settings through the rail gear.
  await gear.click()
  await expect(page).toHaveURL(/\/settings/)
  // The gear is active on the Settings view.
  await expect(gear).toHaveAttribute('aria-pressed', 'true')

  // Click "Shoka" (the all-projects home) in the top bar.
  await page.getByRole('link', { name: 'All projects' }).click()
  // Back on the projects list…
  await expect(page).toHaveURL(/localhost:\d+\/$/)
  // …and the gear is NO LONGER active (the bug: it used to stay active here).
  await expect(gear).toHaveAttribute('aria-pressed', 'false')
})

test('rail parity: the active item matches the shown view across Settings ↔ projects', async ({
  page,
}) => {
  await page.goto('/')
  const gear = page.getByRole('button', { name: 'Settings' })

  // Open Settings → gear active.
  await gear.click()
  await expect(gear).toHaveAttribute('aria-pressed', 'true')
  // Return to projects → gear inactive.
  await page.getByRole('link', { name: 'All projects' }).click()
  await expect(gear).toHaveAttribute('aria-pressed', 'false')
  // Re-open Settings → gear active again (parity holds, no stuck state).
  await gear.click()
  await expect(gear).toHaveAttribute('aria-pressed', 'true')
})
