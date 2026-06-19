import { test, expect, type Page, type Locator } from '@playwright/test'
import { backendCreateProject, backendWrite, backendRead } from './control'

// B-31 (RE-OPEN) — drag-to-trash, PROVEN in a REAL browser. The 5be545d fix passed
// only a jsdom unit seam (which can't render react-arborist rows); on a real browser
// drag-to-trash was fully broken. These run against the real Shoka binary + the real
// production bundle and drive a REAL mouse drag (Playwright dragTo: mouse down →
// move → up), which this Chromium initiates as a native HTML5 drag — so react-arborist
// / react-dnd drive it end-to-end.
//
// THROWAWAY project only (default/trash-dnd-test) — never live data; the dragged
// files are fixtures created over the control socket. No real file is ever deleted:
// the grace is never allowed to elapse; the assertion is that the item ENQUEUES.
//
// ROOT CAUSE proven here (event traces from the instrumented drag):
//  - 5be545d (RED): a real drag over the trash box fires dragstart→dragenter→
//    dragover→DRAGLEAVE→dragend with NO `drop` — the browser refuses to fire `drop`
//    because react-dnd set dropEffect="none" over the non-react-dnd rail. 5be545d's
//    dragend-fallback also fails: `dragleave` fires BEFORE `dragend`, clearing its
//    overTrash flag first. → nothing enqueues (the operator's report).
//  - the fix (GREEN): the trash box is a first-class react-dnd drop target sharing
//    react-arborist's manager, so the browser fires `drop` and react-dnd delivers it.

const NS = 'default'
const PROJ = 'trash-dnd-test'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'junk.md', '# junk\n')
  await backendWrite(NS, PROJ, 'movable.md', '# movable\n')
  await backendWrite(NS, PROJ, 'dropdir/keep.md', '# anchor\n')
})

async function openProject(page: Page, path: string) {
  await page.goto(`/p/${NS}/${PROJ}/blob/${path}`)
}

// Records every drag event on the row / rail, so the root cause is named from
// evidence (which events fire/don't), not hypothesised.
async function instrument(page: Page) {
  await page.evaluate(() => {
    ;(window as unknown as { __dnd: string[] }).__dnd = []
    const log = (window as unknown as { __dnd: string[] }).__dnd
    for (const ty of ['dragstart', 'dragenter', 'dragover', 'dragleave', 'drop', 'dragend']) {
      window.addEventListener(
        ty,
        (e) => {
          const el = e.target as HTMLElement
          const where = el?.closest?.('[aria-label="Trash"]')
            ? 'TRASH'
            : el?.closest?.('[class*="row"]')
              ? 'ROW'
              : (el?.tagName ?? '?')
          log.push(`${ty}@${where}`)
        },
        true,
      )
    }
  })
}
const dndLog = (page: Page) =>
  page.evaluate(() => (window as unknown as { __dnd: string[] }).__dnd)

test('drag a tree row onto the trash box → it enqueues (real browser; RED on 5be545d)', async ({
  page,
}) => {
  await openProject(page, 'junk.md')
  const sidebar = page.locator('#sidebar')
  const row = sidebar.getByText('junk.md', { exact: true })
  const trash = page.getByRole('button', { name: 'Trash' })
  await expect(row).toBeVisible()
  await expect(trash).toBeVisible()

  await instrument(page)
  await row.dragTo(trash) // real native HTML5 drag

  // The dragged file appears in the trash pane with a live countdown. On 5be545d the
  // real drag fires no `drop` on the rail, so this never appears (the RED).
  await expect(page.getByText(/Deleting in \d+s/)).toBeVisible({ timeout: 5000 })
  // Evidence: the browser fired a real `drop` on the trash box.
  expect((await dndLog(page)).some((e) => e.startsWith('drop@TRASH'))).toBe(true)
})

test('drag a row onto a FOLDER is an in-tree move, NOT a trash enqueue (no collateral delete)', async ({
  page,
}) => {
  await openProject(page, 'movable.md')
  const sidebar = page.locator('#sidebar')
  const source = sidebar.getByText('movable.md', { exact: true })
  const target = sidebar.getByText('dropdir', { exact: true }).first()
  await expect(source).toBeVisible()
  await expect(target).toBeVisible()

  await source.dragTo(target)

  // The file moved under dropdir/ (the open view follows) …
  await expect(page).toHaveURL(new RegExp('/blob/dropdir/movable.md$'), {
    timeout: 10_000,
  })
  // … and was NOT enqueued for deletion (no countdown, trash empty).
  await expect(page.getByText(/Deleting in \d+s/)).toHaveCount(0)
})

test('REJECTION: a FOLDER cannot be dragged onto the trash → no enqueue, folder intact (real browser)', async ({
  page,
}) => {
  // Reached the way a user reaches it: open the project, the root folder row
  // `dropdir` is rendered in the tree, the trash box is on the rail — no deep
  // page.goto to the offending row. Folders are NOT draggable (FileTree
  // disableDrag={!isFile}); react-arborist gates this in canDrag, so a real drag
  // of a folder never begins and the trash never receives a NODE — the refusal is
  // behavioural (no enqueue), not a DOM attribute (every row carries draggable=true).
  await openProject(page, 'junk.md')
  const sidebar = page.locator('#sidebar')
  const folder = sidebar.getByText('dropdir', { exact: true }).first()
  const trash = page.getByRole('button', { name: 'Trash' })
  await expect(folder).toBeVisible()
  await expect(trash).toBeVisible()
  // Idle: nothing queued.
  await expect(page.getByText(/Deleting in \d+s/)).toHaveCount(0)

  // Attempt the offending drag: the folder onto the trash box.
  await folder.dragTo(trash)
  // Settle: give any (wrongful) enqueue round-trip + toast time to surface, so the
  // negative assertions below are not racing an async event that hasn't fired yet.
  await page.waitForTimeout(1000)

  // Refused at the SOURCE: the folder is not draggable, so the drag never begins
  // and the trash never even ATTEMPTS an enqueue — no countdown, and crucially no
  // "Could not queue" toast (which the enqueue guard would emit if a folder path
  // ever reached it). On a relaxed disableDrag (the RED) the folder becomes
  // draggable → the drag reaches enqueuePath → readFileFresh rejects the directory
  // → the "Could not queue …" toast appears, flipping this assertion.
  await expect(page.getByText(/Deleting in \d+s/)).toHaveCount(0)
  await expect(page.getByText(/Could not queue .* for deletion/i)).toHaveCount(0)
  await expect(sidebar.getByText('dropdir', { exact: true }).first()).toBeVisible()
  // Backend unchanged: the folder's file is still there (no deletion happened).
  expect(await backendRead(NS, PROJ, 'dropdir/keep.md')).toContain('anchor')
})

test('the trash box shows its drop affordance while a valid drag hovers it', async ({
  page,
}) => {
  await openProject(page, 'junk.md')
  const sidebar = page.locator('#sidebar')
  const row: Locator = sidebar.getByText('junk.md', { exact: true })
  const trash = page.getByRole('button', { name: 'Trash' })
  await expect(row).toBeVisible()
  await expect(trash).toBeVisible()

  // Idle: no affordance.
  await expect(trash).toHaveAttribute('data-drop-active', 'false')

  // Begin a react-dnd drag and HOLD it over the trash box (dragstart→dragenter→
  // dragover, no drop). react-dnd's monitor.isOver() drives the affordance. (The
  // affordance is a pure react-dnd hover-state assertion; the actual enqueue is
  // proven by the real dragTo test above. On 5be545d the rail is not a react-dnd
  // target, so data-drop-active is always false — RED.)
  const s = await row.elementHandle()
  const t = await trash.elementHandle()
  if (!s || !t) throw new Error('row/trash not found')
  await page.evaluate(
    ([src, dst]) => {
      const dt = new DataTransfer()
      const fire = (el: Element, type: string) => {
        const r = el.getBoundingClientRect()
        el.dispatchEvent(
          new DragEvent(type, {
            bubbles: true,
            cancelable: true,
            dataTransfer: dt,
            clientX: r.left + r.width / 2,
            clientY: r.top + r.height / 2,
          }),
        )
      }
      fire(src as Element, 'dragstart')
      fire(dst as Element, 'dragenter')
      fire(dst as Element, 'dragover')
    },
    [s, t],
  )

  await expect(trash).toHaveAttribute('data-drop-active', 'true')

  // Cleanup: end the drag off the rail so no enqueue happens.
  await page.evaluate(() => {
    document.body.dispatchEvent(new DragEvent('dragend', { bubbles: true }))
  })
})
