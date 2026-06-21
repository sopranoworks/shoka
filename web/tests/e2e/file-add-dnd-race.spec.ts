import { test, expect, type Page } from '@playwright/test'
import { backendCreateProject, backendWrite } from './control'
import { dropFile, legacyDropFolder } from './dndHelpers'

// B-28 file-add-dnd rootcause — the DETERMINISTIC RED→GREEN proof of the resolve-then-
// dispatch race that surfaced on the ab9286b full gate (file-add-dnd.spec.ts:96 failed
// once, then passed on re-run/isolation — which is NOT proof of a fix).
//
// ROOT CAUSE (file:line): the folder drop's destination is COORDINATE-resolved by the
// product — FileDropzone.resolveDestDir → elementFromPoint(dropOffset).closest(
// '[data-dir-path]') (FileDropzone.tsx:26-31). The pre-fix helper measured the folder
// row's box in a SEPARATE Playwright call (T1) then dispatched the drop in a LATER
// evaluate (T2). A tree relayout/remount in the T1→T2 gap (e.g. the mount-time react-
// arborist ResizeObserver setSize, FileTree.tsx:96-105) moves the row, so the precomputed
// point is stale → the product resolves the drop to the WRONG directory (root). c2f19a2
// only gated the POST-drop navigation on the settle toast; it never closed this PRE-drop
// resolve-then-dispatch window — so a second, independent race remained.
//
// INJECTED DELAY (the deterministic reproduction, directive §2.5 hook): a test-scoped,
// default-off sidebar SHIFT applied in the T1→T2 gap stands in for the real relayout,
// making the stale-point failure happen EVERY time — not a repeat-count. The fix
// (dndHelpers.dropFile) resolves + dispatches ATOMICALLY after the row settles, so it
// re-reads the row's CURRENT rect and is immune to the SAME injected shift.

const NS = 'default'
const PROJ = 'dnd-race'
const SHIFT = 30

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'README.md', '# Root\n')
  await backendWrite(NS, PROJ, 'dropdir/keep.md', '# keep\n')
})

async function openProject(page: Page) {
  await page.goto(`/p/${NS}/${PROJ}/blob/README.md`)
  await expect(page.getByTestId('file-dropzone')).toBeVisible()
  // Ensure the folder row is present before the proof manipulates layout.
  await expect(page.locator('#sidebar').getByText('dropdir', { exact: true })).toBeVisible()
}

// RED — the PRE-FIX (non-atomic) helper under the injected shift: the point measured at
// T1 is stale after the shift, so the drop resolves to the project ROOT, not dropdir.
// This FAILS every time under the injection (it is the bug, reproduced deterministically).
test('RED: legacy resolve-then-dispatch drops into the WRONG dir under an injected relayout', async ({
  page,
}) => {
  await openProject(page)
  await legacyDropFolder(page, 'red-in-folder.md', '# In folder\n', 'dropdir', SHIFT)
  // The drop landed at the ROOT (wrong) because the stale point missed the shifted row:
  // the settle toast names the root path, and the intended dropdir path was NOT created.
  await expect(page.getByText('Added red-in-folder.md')).toBeVisible({ timeout: 10_000 })
  await expect(page.getByText(/dropdir\/red-in-folder\.md/)).toHaveCount(0)
})

// GREEN — the FIX (atomic resolve+dispatch) under the SAME injected shift: it re-reads the
// row's current rect at dispatch time, so the drop lands in dropdir as intended.
test('GREEN: atomic resolve+dispatch lands in dropdir under the SAME injected relayout', async ({
  page,
}) => {
  process.env.DND_INJECT_SHIFT_PX = String(SHIFT) // same injected relayout as the RED case
  try {
    await openProject(page)
    await dropFile(page, 'green-in-folder.md', '# In folder\n', 'dropdir')
    await expect(
      page.getByText(/(Added|Kept the existing) dropdir\/green-in-folder\.md/),
    ).toBeVisible({ timeout: 10_000 })
    await page.goto(`/p/${NS}/${PROJ}/blob/dropdir/green-in-folder.md`)
    await expect(page.getByText('In folder', { exact: false })).toBeVisible({ timeout: 10_000 })
  } finally {
    delete process.env.DND_INJECT_SHIFT_PX
  }
})
