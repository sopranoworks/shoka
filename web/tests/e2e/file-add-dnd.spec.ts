import { test, expect, type Page } from '@playwright/test'
import { backendCreateProject, backendWrite } from './control'
import { dropFile } from './dndHelpers'

// B-28 — external file drag-and-drop ADD, PROVEN in a REAL browser (the trash-D&D
// class: a unit/mock seam can be green while the real browser path is broken). These
// run against the real Shoka binary + the real production bundle and drive a REAL
// native OS-file drop (a DataTransfer carrying a File, dispatched as
// dragenter→dragover→drop) onto the explorer dropzone, exercising the real /ws/ui
// SAVE_FILE base64 round-trip end to end.
//
// THROWAWAY project only (default/file-add-test) — never live data; the dropped
// files are synthetic File objects built in the page, never real OS files.
//
// RED PROOF (performed during development, then reverted): removing the
// <FileDropzone> wrapper in Sidebar.tsx (or dropping the content_encoding handling
// in handleSaveFile) turns the "adds a file" test RED — the drop does nothing and
// the new row never appears. Restoring the wiring turns it GREEN, confirming the
// test exercises the real drop path, not a mock.

const NS = 'default'
const PROJ = 'file-add-test'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'README.md', '# Root\n')
  await backendWrite(NS, PROJ, 'dropdir/keep.md', '# keep\n')
  await backendWrite(NS, PROJ, 'collide-accept.md', 'ORIGINAL ACCEPT\n')
  await backendWrite(NS, PROJ, 'collide-cancel.md', 'ORIGINAL CANCEL\n')
})

async function openProject(page: Page, path: string) {
  await page.goto(`/p/${NS}/${PROJ}/blob/${path}`)
  await expect(page.getByTestId('file-dropzone')).toBeVisible()
}

// dropFile (the ATOMIC, race-free native-drop helper) now lives in ./dndHelpers so the
// rootcause proof spec can reuse it; see that file for the resolve-then-dispatch race fix.

test('drops an allowlisted .md onto the explorer → it is ADDED to the tree (real browser; core)', async ({
  page,
}) => {
  await openProject(page, 'README.md')

  await dropFile(page, 'added-root.md', '# Added by drop\n')

  // The new file appears in the real tree (the SAVE_FILE round-trip landed and the
  // GET_TREE query refreshed). On a broken dropzone this never appears (the RED).
  await expect(
    page.locator('#sidebar').getByText('added-root.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })
})

test('drops a file onto a FOLDER row → it lands under that folder', async ({ page }) => {
  await openProject(page, 'README.md')

  await dropFile(page, 'in-folder.md', '# In folder\n', 'dropdir')

  // Wait for the async drop → SAVE_FILE round-trip to actually SETTLE before navigating.
  // The per-file toast is emitted only after addDroppedFile resolves (the save committed
  // AND the tree query was invalidated). dropFile only DISPATCHES the drop and returns;
  // navigating before the save completes would tear the page down mid-save and the file
  // would never be created — the historical flake. This waits on the real completion
  // signal (duration-independent), accepting either outcome — a fresh "Added …" or, when
  // the file already exists (e.g. under --repeat-each), the "Kept the existing …" settle.
  await expect(
    page.getByText(/(Added|Kept the existing) dropdir\/in-folder\.md/),
  ).toBeVisible({ timeout: 10_000 })

  // It was created at dropdir/in-folder.md (folder-targeted destination): the blob
  // route for that exact path loads and renders the dropped content.
  await page.goto(`/p/${NS}/${PROJ}/blob/dropdir/in-folder.md`)
  await expect(page.getByText('In folder', { exact: false })).toBeVisible({
    timeout: 10_000,
  })
})

test('drops a NON-allowlisted file (.png) → it is rejected and NOT added', async ({
  page,
}) => {
  await openProject(page, 'README.md')

  await dropFile(page, 'photo.png', 'binary-ish')

  // A clear rejection is surfaced …
  await expect(page.getByText(/was not added/i)).toBeVisible({ timeout: 5000 })
  // … and nothing was added to the tree.
  await expect(
    page.locator('#sidebar').getByText('photo.png', { exact: true }),
  ).toHaveCount(0)
})

test('a name collision is NOT silently overwritten — confirm overwrites, cancel keeps the original (core)', async ({
  page,
}) => {
  // CANCEL first: dismiss the confirm → the original content is kept.
  await openProject(page, 'collide-cancel.md')
  await expect(page.getByText('ORIGINAL CANCEL', { exact: false })).toBeVisible()
  page.once('dialog', (d) => void d.dismiss())
  await dropFile(page, 'collide-cancel.md', '# SHOULD NOT LAND\n')
  // Wait until the declined flow has fully SETTLED (the conflict round-trip completed and
  // the user declined → the "Kept the existing …" toast) before navigating — not a fixed
  // beat. Only then assert the original is untouched.
  await expect(page.getByText('Kept the existing collide-cancel.md')).toBeVisible({ timeout: 10_000 })
  await page.goto(`/p/${NS}/${PROJ}/blob/collide-cancel.md`)
  await expect(page.getByText('ORIGINAL CANCEL', { exact: false })).toBeVisible({
    timeout: 10_000,
  })
  await expect(page.getByText('SHOULD NOT LAND', { exact: false })).toHaveCount(0)

  // ACCEPT: confirm the overwrite → the dropped content replaces the original.
  await openProject(page, 'collide-accept.md')
  await expect(page.getByText('ORIGINAL ACCEPT', { exact: false })).toBeVisible()
  page.once('dialog', (d) => void d.accept())
  await dropFile(page, 'collide-accept.md', '# OVERWRITTEN BY DROP\n')
  // Wait for the confirmed overwrite to LAND (the "Added <path>" toast fires only after
  // the if_match resend committed) before navigating — same real-signal gate as above.
  await expect(page.getByText('Added collide-accept.md')).toBeVisible({ timeout: 10_000 })
  await page.goto(`/p/${NS}/${PROJ}/blob/collide-accept.md`)
  await expect(page.getByText('OVERWRITTEN BY DROP', { exact: false })).toBeVisible({
    timeout: 10_000,
  })
})
