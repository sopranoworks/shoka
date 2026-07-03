import { test, expect, type Page } from '@playwright/test'
import { backendWrite, backendMove, backendRead, backendCreateProject } from './control'

// Move / rename (Obsidian-model), against the real binary + /ws/ui. Each test
// seeds its OWN files under _e2e/ via the control socket. A move is a PURE PATH
// CHANGE — these tests also assert NO link surface ever appears.

const NS = 'demo'
const PROJ = 'docs'
const blobUrl = (p: string) => `/p/${NS}/${PROJ}/blob/${p}`
const editUrl = (p: string) => `/p/${NS}/${PROJ}/edit/${p}`
const seed = (path: string, content: string) => backendWrite(NS, PROJ, path, content)

async function openPalette(page: Page, command: string) {
  await page.keyboard.press('Meta+k')
  const input = page.getByPlaceholder('Type a command or search…')
  await expect(input).toBeVisible()
  await input.fill(command)
  await page.keyboard.press('Enter')
}

async function replaceBuffer(page: Page, text: string) {
  await page.locator('.cm-content').click()
  await page.keyboard.press('Meta+a')
  await page.keyboard.type(text)
}

// No link surface anywhere — the path-only guarantee, observable. Targets the
// specific link-rewrite phrasings the move must NEVER show (a count, a
// will-be-updated pre-scan, a rewritten note), not the bare word "link" (which
// would false-match the unrelated "Copy deep link" affordance).
async function expectNoLinkSurface(page: Page) {
  await expect(page.getByText(/\d+\s+links?\b/i)).toHaveCount(0)
  await expect(page.getByText(/links?\s+(will be|were|updated|rewritten)/i)).toHaveCount(0)
  await expect(page.getByText(/will be updated/i)).toHaveCount(0)
  await expect(page.getByText(/rewritten/i)).toHaveCount(0)
}

test('dialog Move: type a destination path → file relocates and the view follows', async ({
  page,
}) => {
  await seed('_e2e/movesrc.md', '# Move Me\n')
  await page.goto(blobUrl('_e2e/movesrc.md'))
  await expect(page.getByRole('heading', { name: 'Move Me' })).toBeVisible()

  await openPalette(page, 'Move file')
  const dialog = page.getByRole('dialog')
  await expect(dialog.locator('input')).toHaveValue('_e2e/movesrc.md')
  await expectNoLinkSurface(page) // no "N links" pre-scan in the dialog
  await dialog.locator('input').fill('_e2e/moved/dest.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/moved/dest.md$'))
  await expect(page.getByRole('heading', { name: 'Move Me' })).toBeVisible()
  await expectNoLinkSurface(page) // no "N links updated" after the move
})

test('dialog Rename: keeps the directory, swaps the basename', async ({ page }) => {
  await seed('_e2e/renamesrc.md', '# Rename Me\n')
  await page.goto(blobUrl('_e2e/renamesrc.md'))
  await openPalette(page, 'Rename file')

  const dialog = page.getByRole('dialog')
  await expect(dialog.locator('input')).toHaveValue('renamesrc.md') // basename only
  await dialog.locator('input').fill('renamed.md')
  await dialog.getByRole('button', { name: 'Rename' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/renamed.md$'))
  await expect(page.getByRole('heading', { name: 'Rename Me' })).toBeVisible()
})

test('collision → Overwrite (inline-confirm) replaces the target', async ({ page }) => {
  await seed('_e2e/col-src.md', '# Source Wins\n')
  await seed('_e2e/col-dst.md', '# Destination\n')
  await page.goto(blobUrl('_e2e/col-src.md'))

  await openPalette(page, 'Move file')
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/col-dst.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  const warn = page.getByRole('alertdialog')
  await expect(warn).toContainText('already exists')
  await expectNoLinkSurface(page)
  await warn.getByRole('button', { name: 'Overwrite' }).click()
  await warn.getByRole('button', { name: 'Confirm overwrite' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/col-dst.md$'))
  await expect(page.getByRole('heading', { name: 'Source Wins' })).toBeVisible()
})

test('collision → Save under a different name reopens the dialog at a free path', async ({
  page,
}) => {
  await seed('_e2e/col2-src.md', '# Mover\n')
  await seed('_e2e/col2-dst.md', '# Occupied\n')
  await page.goto(blobUrl('_e2e/col2-src.md'))

  await openPalette(page, 'Move file')
  let dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/col2-dst.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: 'Save under a different name' })
    .click()

  // The dialog reopens prefilled with the attempted (occupied) target.
  dialog = page.getByRole('dialog')
  await expect(dialog.locator('input')).toHaveValue('_e2e/col2-dst.md')
  await dialog.locator('input').fill('_e2e/col2-free.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/col2-free.md$'))
  await expect(page.getByRole('heading', { name: 'Mover' })).toBeVisible()
})

test('collision → Cancel aborts with no change', async ({ page }) => {
  await seed('_e2e/col3-src.md', '# Stay\n')
  await seed('_e2e/col3-dst.md', '# Keep\n')
  await page.goto(blobUrl('_e2e/col3-src.md'))

  await openPalette(page, 'Move file')
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/col3-dst.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)
  await expect(page).toHaveURL(new RegExp('/blob/_e2e/col3-src.md$'))
})

test('a second connection moving the open file makes the blob view follow', async ({
  page,
}) => {
  await seed('_e2e/follow.md', '# Followed\n')
  await page.goto(blobUrl('_e2e/follow.md'))
  await expect(page.getByRole('heading', { name: 'Followed' })).toBeVisible()

  await backendMove(NS, PROJ, '_e2e/follow.md', '_e2e/followed-here.md')

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/followed-here.md$'))
  await expect(page.getByRole('heading', { name: 'Followed' })).toBeVisible()
  await expectNoLinkSurface(page)
})

test('the edit route follows a move buffer-safe: unsaved edits survive', async ({
  page,
}) => {
  await seed('_e2e/efollow.md', '# Original\n')
  await page.goto(editUrl('_e2e/efollow.md'))
  await expect(page.locator('.cm-content')).toBeVisible()
  await replaceBuffer(page, '# My Unsaved Edits\n')

  await backendMove(NS, PROJ, '_e2e/efollow.md', '_e2e/efollowed.md')

  // Followed to the new path, still in edit mode, with the dirty buffer intact.
  await expect(page).toHaveURL(new RegExp('/edit/_e2e/efollowed.md$'))
  await expect(page.locator('.cm-content')).toContainText('My Unsaved Edits')
})

test('context menu → Move… opens the move dialog', async ({ page }) => {
  await seed('_e2e/ctxmove.md', '# Ctx Move\n')
  await page.goto(blobUrl('_e2e/ctxmove.md'))
  const sidebar = page.locator('#sidebar')
  const row = sidebar.getByText('ctxmove.md', { exact: true })
  await expect(row).toBeVisible()
  await row.click({ button: 'right' })

  await page.getByRole('menu').getByRole('button', { name: 'Move…' }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.locator('input')).toHaveValue('_e2e/ctxmove.md')
  await dialog.locator('input').fill('_e2e/ctxmoved.md')
  await dialog.getByRole('button', { name: 'Move' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/ctxmoved.md$'))
})

test('context menu → Rename… enters inline edit and commits a move', async ({
  page,
}) => {
  await seed('_e2e/inline.md', '# Inline\n')
  await page.goto(blobUrl('_e2e/inline.md'))
  const sidebar = page.locator('#sidebar')
  const row = sidebar.getByText('inline.md', { exact: true })
  await expect(row).toBeVisible()
  await row.click({ button: 'right' })
  await page.getByRole('menu').getByRole('button', { name: 'Rename…' }).click()

  const input = sidebar.locator('input:not([type="search"])')
  await expect(input).toBeVisible()
  await input.fill('inline-renamed.md')
  await input.press('Enter')

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/inline-renamed.md$'))
})

test('drag a file leaf onto a directory moves it there', async ({ page }) => {
  // Seed a file to drag and a destination directory (created by seeding a file
  // inside it).
  await seed('_e2e/drag/source.md', '# Dragged\n')
  await seed('_e2e/dropdir/keep.md', '# Anchor\n')
  await page.goto(blobUrl('_e2e/drag/source.md'))

  // expand-to-active already opens the ancestors (_e2e, drag) so source.md is
  // shown; dropdir is a sibling directory row, also visible. (Do NOT click the
  // ancestor dirs — that would toggle them closed.)
  const sidebar = page.locator('#sidebar')
  const source = sidebar.getByText('source.md', { exact: true })
  const target = sidebar.getByText('dropdir', { exact: true }).first()
  await expect(source).toBeVisible()
  await expect(target).toBeVisible()

  await htmlDragAndDrop(page, source, target)

  // The move lands the file under dropdir/ and the open view follows.
  await expect(page).toHaveURL(new RegExp('/blob/_e2e/dropdir/source.md$'), {
    timeout: 10_000,
  })
})

test('REJECTION (drag): dropping onto a folder that already holds the name → collision warning, Cancel leaves it put (real browser)', async ({
  page,
}) => {
  // Own throwaway project so the tree stays SMALL — the click-through reach below
  // navigates a virtualized tree, and the shared demo/docs _e2e/ is polluted by
  // the other move tests (the source row would virtualize out of view).
  const RNS = 'default'
  const RPROJ = 'move-reject-test'
  await backendCreateProject(RNS, RPROJ)
  // A root file to land on neutrally (no expand-to-active), then two sibling
  // folders: a source holding `dragme.md` and an occupied dest also holding it.
  await backendWrite(RNS, RPROJ, 'README.md', '# Root\n')
  await backendWrite(RNS, RPROJ, 'dcsrc/dragme.md', '# DRAG SOURCE\n')
  await backendWrite(RNS, RPROJ, 'dcdst/dragme.md', '# OCCUPIED\n')

  // Reach the source THROUGH THE NORMAL UI PATH: open the neutral root file (so
  // expand-to-active does NOT auto-open the folders) and navigate the rendered
  // tree by CLICKING to expand — not a deep page.goto to the source blob.
  await page.goto(`/p/${RNS}/${RPROJ}/blob/README.md`)
  const sidebar = page.locator('#sidebar')
  await sidebar.getByText('dcsrc', { exact: true }).first().click() // expand dcsrc
  const source = sidebar.getByText('dragme.md', { exact: true })
  const target = sidebar.getByText('dcdst', { exact: true }).first() // collapsed dest folder
  await expect(source).toBeVisible()
  await expect(target).toBeVisible()

  // The real offending drop.
  await htmlDragAndDrop(page, source, target)

  // Refused: the three-action collision warning appears (no silent overwrite).
  const warn = page.getByRole('alertdialog')
  await expect(warn).toContainText('already exists', { timeout: 10_000 })
  await expectNoLinkSurface(page)
  await warn.getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByRole('alertdialog')).toHaveCount(0)

  // Backend unchanged: the dest keeps its content and the source still exists at
  // its original path (nothing moved, nothing overwritten). On a broken
  // no-overwrite guard (the RED) the warning never shows and the dest is silently
  // replaced.
  expect(await backendRead(RNS, RPROJ, 'dcdst/dragme.md')).toContain('OCCUPIED')
  expect(await backendRead(RNS, RPROJ, 'dcsrc/dragme.md')).toContain('DRAG SOURCE')
})

// react-dnd's HTML5 backend listens to native HTML5 drag events, which
// Playwright's mouse-based dragTo does not emit. Dispatch a synthetic
// dragstart→dragover→drop→dragend sequence sharing one DataTransfer.
async function htmlDragAndDrop(
  page: Page,
  source: ReturnType<Page['locator']>,
  target: ReturnType<Page['locator']>,
) {
  const s = await source.elementHandle()
  const t = await target.elementHandle()
  if (!s || !t) throw new Error('drag source/target not found')
  await page.evaluate(
    ([src, dst]) => {
      const dt = new DataTransfer()
      const fire = (el: Element, type: string) => {
        const r = el.getBoundingClientRect()
        const ev = new DragEvent(type, {
          bubbles: true,
          cancelable: true,
          dataTransfer: dt,
          clientX: r.left + r.width / 2,
          clientY: r.top + r.height / 2,
        })
        el.dispatchEvent(ev)
      }
      fire(src as Element, 'dragstart')
      fire(dst as Element, 'dragenter')
      fire(dst as Element, 'dragover')
      fire(dst as Element, 'drop')
      fire(src as Element, 'dragend')
    },
    [s, t],
  )
}
