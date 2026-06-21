import { expect, type Page } from '@playwright/test'

// Shared native-file-drop helpers for the file-add DnD specs (B-28).
//
// The destination folder is COORDINATE-resolved by the product: FileDropzone.resolveDestDir
// does elementFromPoint(dropOffset).closest('[data-dir-path]') (FileDropzone.tsx:26-31), so
// a folder-targeted drop's clientX/Y must match the folder row's CURRENT screen position at
// drop time. `dropFile` resolves the target AND dispatches the synthetic native drop
// ATOMICALLY in one in-page evaluate, after rAF-waiting for the row to be present with a
// STABLE non-zero rect — so the coordinates can never be stale relative to the dispatch (the
// resolve-then-dispatch race the legacy helper had).

// TEST-SCOPED, DEFAULT-OFF injectable race hook (directive §2.5): DND_INJECT_SHIFT_PX > 0
// shifts the sidebar at the race window so a captured firing can be made deterministic. It
// lives ONLY in this test helper and is never read by product code / never shipped.
export function injectShiftPx(): number {
  return Number(process.env.DND_INJECT_SHIFT_PX ?? 0)
}

// dropFile (ATOMIC, race-free): resolve target + dispatch in one evaluate after the row
// settles. Immune to a tree relayout/remount because it re-reads the rect at dispatch time.
export async function dropFile(page: Page, name: string, content: string, onFolder?: string) {
  await page.evaluate(
    async ({ name, content, onFolder, shiftPx }) => {
      const findTarget = (): HTMLElement | null =>
        onFolder
          ? document.querySelector<HTMLElement>(`#sidebar [data-dir-path="${onFolder}"]`)
          : document.querySelector<HTMLElement>('[data-testid="file-dropzone"]')
      const frame = () => new Promise<void>((r) => requestAnimationFrame(() => r()))
      const stable = async (): Promise<HTMLElement> => {
        const deadline = performance.now() + 10000
        let prev: DOMRect | null = null
        for (;;) {
          const el = findTarget()
          if (el) {
            const r = el.getBoundingClientRect()
            if (
              r.width > 0 &&
              r.height > 0 &&
              prev &&
              Math.abs(r.x - prev.x) < 0.5 &&
              Math.abs(r.y - prev.y) < 0.5
            ) {
              return el
            }
            prev = r
          } else {
            prev = null
          }
          if (performance.now() > deadline)
            throw new Error(`drop target not stable: ${onFolder ?? 'dropzone'}`)
          await frame()
        }
      }
      const el = await stable()
      // Injected relayout AFTER resolve (non-destructive transform — no DOM insert, no
      // React re-render): a non-atomic helper would now hold a stale point; the atomic
      // helper re-reads the rect below, so it stays correct.
      if (shiftPx > 0) {
        const sb = document.querySelector<HTMLElement>('#sidebar')
        if (sb) sb.style.transform = `translateY(${shiftPx}px)`
        await frame()
      }
      const r = el.getBoundingClientRect()
      const point = onFolder
        ? { x: r.x + r.width / 2, y: r.y + r.height / 2 }
        : { x: r.x + r.width / 2, y: r.bottom - 6 }
      const targetEl = document.elementFromPoint(point.x, point.y) ?? el
      const dt = new DataTransfer()
      dt.items.add(new File([content], name, { type: 'text/markdown' }))
      const opts = {
        bubbles: true,
        cancelable: true,
        dataTransfer: dt,
        clientX: point.x,
        clientY: point.y,
      }
      targetEl.dispatchEvent(new DragEvent('dragenter', opts))
      targetEl.dispatchEvent(new DragEvent('dragover', opts))
      targetEl.dispatchEvent(new DragEvent('drop', opts))
    },
    { name, content, onFolder, shiftPx: injectShiftPx() },
  )
}

// legacyDropFolder is the PRE-FIX, non-atomic helper, kept ONLY to demonstrate the race in
// the rootcause proof spec: it measures the folder row's box in a separate Playwright call
// (T1), then dispatches at that point in a later evaluate (T2). With shiftPx > 0 the sidebar
// shifts in the T1→T2 gap, so the precomputed point is stale → the product resolves the drop
// to the WRONG directory (root). Do NOT use this in real specs.
export async function legacyDropFolder(
  page: Page,
  name: string,
  content: string,
  folder: string,
  shiftPx: number,
) {
  const row = page.locator('#sidebar').getByText(folder, { exact: true }).first()
  await expect(row).toBeVisible()
  const box = await row.boundingBox() // T1 — measured in a separate call
  if (!box) throw new Error(`folder row ${folder} has no box`)
  const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 }
  // The race window: the layout changes between measuring (T1) and dispatching (T2). A
  // non-destructive transform moves the rows without a DOM insert / React re-render.
  if (shiftPx > 0) {
    await page.evaluate((px) => {
      const sb = document.querySelector<HTMLElement>('#sidebar')
      if (sb) sb.style.transform = `translateY(${px}px)`
    }, shiftPx)
  }
  await page.evaluate(
    ({ name, content, point }) => {
      const el = document.elementFromPoint(point.x, point.y) // T2 — stale point after the shift
      if (!el) throw new Error('no element at drop point')
      const dt = new DataTransfer()
      dt.items.add(new File([content], name, { type: 'text/markdown' }))
      const opts = {
        bubbles: true,
        cancelable: true,
        dataTransfer: dt,
        clientX: point.x,
        clientY: point.y,
      }
      el.dispatchEvent(new DragEvent('dragenter', opts))
      el.dispatchEvent(new DragEvent('dragover', opts))
      el.dispatchEvent(new DragEvent('drop', opts))
    },
    { name, content, point },
  )
}
