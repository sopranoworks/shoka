import { test, expect } from '@playwright/test'
import { backendCreateProject, backendWrite, backendDelete } from './control'

// B-28 (the 2026-06-18 deleted-log directive) — the admin "Deleted files" view +
// forward-only REVIVE, PROVEN in a REAL browser against the real Shoka binary +
// production bundle + the real /ws/ui LIST_DELETED / REVIVE_FILE round-trip.
//
// THROWAWAY project only (default/deleted-revive-test) — never live data. The file
// is a fixture created + deleted over the control socket; revival re-creates it as
// a NEW commit (forward-only), so nothing is destroyed.
//
// RED-proof: with the REVIVE_FILE wiring broken (handler/op removed) the Revive
// click yields an error toast and the file never reappears — the success assertion
// below fails. GREEN with the wiring in place.

const NS = 'default'
const PROJ = 'deleted-revive-test'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'survivor.md', '# survivor\n')
  await backendWrite(NS, PROJ, 'gone.md', '# the original content\n')
  // Delete it so it lands in the deleted-file log.
  await backendDelete(NS, PROJ, 'gone.md')
})

test('admin sees the deleted file and revives it (real browser, real round-trip)', async ({
  page,
}) => {
  await page.goto(`/p/${NS}/${PROJ}/deleted`)

  // The cheap LIST_DELETED read shows the deleted file (a real round-trip).
  const row = page.getByTestId('deleted-row-gone.md')
  await expect(row).toBeVisible({ timeout: 10_000 })
  await expect(row).toContainText('gone.md')

  // Revive it → the real REVIVE_FILE op re-creates it forward-only.
  await page.getByTestId('revive-gone.md').click()

  // The row disappears from the deleted list once the revive commit lands and the
  // list refetches (it is no longer deleted).
  await expect(page.getByTestId('deleted-row-gone.md')).toHaveCount(0, {
    timeout: 10_000,
  })

  // And the file is back in the project: its content is readable again at the blob
  // route with the ORIGINAL bytes (forward-only revive restored the last content).
  await page.goto(`/p/${NS}/${PROJ}/blob/gone.md`)
  await expect(page.getByText('the original content')).toBeVisible({ timeout: 10_000 })
})
