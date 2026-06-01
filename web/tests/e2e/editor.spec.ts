import { test, expect, type Page } from '@playwright/test'
import { backendWrite } from './control'

// Session-3 editor, against the real binary + /ws/ui (see global-setup.ts).
// Each test seeds its OWN file under _e2e/ via the control socket, so saves
// never mutate the shared fixture files the other specs depend on.

const NS = 'demo'
const PROJ = 'docs'
const editUrl = (p: string) => `/p/${NS}/${PROJ}/edit/${p}`
const blobUrl = (p: string) => `/p/${NS}/${PROJ}/blob/${p}`

async function seed(path: string, content: string) {
  await backendWrite(NS, PROJ, path, content)
}

// Replace the whole CodeMirror buffer with `text`.
async function replaceBuffer(page: Page, text: string) {
  const content = page.locator('.cm-content')
  await content.click()
  await page.keyboard.press('Meta+a')
  await page.keyboard.type(text)
}

const toolbarSave = (page: Page) =>
  page.getByRole('main').getByRole('button', { name: 'Save', exact: true })

test('happy path: view → Edit → modify → Save → back at view with new content', async ({
  page,
}) => {
  await seed('_e2e/happy.md', '# Seed\n')
  await page.goto(blobUrl('_e2e/happy.md'))
  await page.getByRole('button', { name: 'Edit' }).click()
  await expect(page).toHaveURL(new RegExp('/edit/_e2e/happy.md$'))

  await replaceBuffer(page, '# Edited Heading\n\nnew body\n')
  await toolbarSave(page).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/happy.md$'))
  await expect(page.getByRole('heading', { name: 'Edited Heading' })).toBeVisible()
})

test('cancel with dirty buffer prompts, and confirming returns to the view unchanged', async ({
  page,
}) => {
  await seed('_e2e/cancel.md', '# Keep Me\n')
  await page.goto(editUrl('_e2e/cancel.md'))
  await replaceBuffer(page, 'scratch edits that should be discarded')
  await page.getByRole('button', { name: 'Cancel' }).click()

  await expect(page.getByText('Discard unsaved changes?')).toBeVisible()
  await page.getByRole('button', { name: 'Discard changes' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/cancel.md$'))
  await expect(page.getByRole('heading', { name: 'Keep Me' })).toBeVisible()
})

test('cancel with a clean buffer navigates immediately (no prompt)', async ({
  page,
}) => {
  await seed('_e2e/cancelclean.md', '# Clean\n')
  await page.goto(editUrl('_e2e/cancelclean.md'))
  await expect(page.locator('.cm-content')).toBeVisible()
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(page).toHaveURL(new RegExp('/blob/_e2e/cancelclean.md$'))
  await expect(page.getByText('Discard unsaved changes?')).toHaveCount(0)
})

test('conflict — Discard mine loads the latest server content', async ({ page }) => {
  await seed('_e2e/discard.md', '# Original\n')
  await page.goto(editUrl('_e2e/discard.md'))
  await replaceBuffer(page, '# Mine\n')

  await seed('_e2e/discard.md', '# Theirs\n\nserver wins\n')

  await toolbarSave(page).click()
  await expect(
    page.getByText('Save failed — this file was modified by someone else.'),
  ).toBeVisible()

  await page.getByRole('button', { name: 'Discard mine, load latest' }).click()
  await expect(
    page.getByText('Save failed — this file was modified by someone else.'),
  ).toHaveCount(0)
  await expect(page.locator('.cm-content')).toContainText('server wins')
})

test('conflict — Force overwrite writes the buffer over the other change', async ({
  page,
}) => {
  await seed('_e2e/force.md', '# Original\n')
  await page.goto(editUrl('_e2e/force.md'))
  await replaceBuffer(page, '# Forced\n\nmy content wins\n')
  await seed('_e2e/force.md', '# Theirs\n')

  await toolbarSave(page).click()
  await page.getByRole('button', { name: 'Force overwrite' }).click()
  await page.getByRole('button', { name: 'Confirm overwrite' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/force.md$'))
  await expect(page.getByRole('heading', { name: 'Forced' })).toBeVisible()
})

test('conflict — Save as a new path creates the file with my content', async ({
  page,
}) => {
  await seed('_e2e/saveas-src.md', '# Original\n')
  await page.goto(editUrl('_e2e/saveas-src.md'))
  await replaceBuffer(page, '# Saved Elsewhere\n')
  await seed('_e2e/saveas-src.md', '# Theirs\n')

  await toolbarSave(page).click()
  await page.getByRole('button', { name: 'Save as…' }).click()

  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/saveas-new.md')
  await dialog.getByRole('button', { name: 'Save' }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/saveas-new.md$'))
  await expect(page.getByRole('heading', { name: 'Saved Elsewhere' })).toBeVisible()
})

test('conflict — Save as an existing path asks to overwrite, then writes it', async ({
  page,
}) => {
  await seed('_e2e/saveas2-src.md', '# Original\n')
  await seed('_e2e/saveas2-dst.md', '# Destination\n')
  await page.goto(editUrl('_e2e/saveas2-src.md'))
  await replaceBuffer(page, '# Into Destination\n')
  await seed('_e2e/saveas2-src.md', '# Theirs\n')

  await toolbarSave(page).click()
  await page.getByRole('button', { name: 'Save as…' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.locator('input').fill('_e2e/saveas2-dst.md')
  await dialog.getByRole('button', { name: 'Save' }).click()

  await expect(page.getByText('Overwrite existing file?')).toBeVisible()
  await page.getByRole('button', { name: 'Overwrite', exact: true }).click()

  await expect(page).toHaveURL(new RegExp('/blob/_e2e/saveas2-dst.md$'))
  await expect(page.getByRole('heading', { name: 'Into Destination' })).toBeVisible()
})

test('conflict — Show diff displays both sides, and closing keeps the banner', async ({
  page,
}) => {
  await seed('_e2e/diff.md', '# Original\n')
  await page.goto(editUrl('_e2e/diff.md'))
  await replaceBuffer(page, '# My Diff Side\n')
  await seed('_e2e/diff.md', '# Their Diff Side\n')

  await toolbarSave(page).click()
  await page.getByRole('button', { name: 'Show diff' }).click()

  const diff = page.getByRole('dialog', { name: /Diff/ })
  await expect(diff).toBeVisible()
  await expect(diff).toContainText('My Diff Side')
  await expect(diff).toContainText('Their Diff Side')

  await diff.getByRole('button', { name: 'Close diff' }).click()
  await expect(page.getByRole('button', { name: 'Force overwrite' })).toBeVisible()
})

test('other-source modify raises a non-blocking banner on the edit route', async ({
  page,
}) => {
  await seed('_e2e/extwrite.md', '# Original\n')
  await page.goto(editUrl('_e2e/extwrite.md'))
  await expect(page.locator('.cm-content')).toBeVisible()

  await seed('_e2e/extwrite.md', '# Changed Underneath\n')
  await expect(
    page.getByText('This file was modified by someone else.'),
  ).toBeVisible()
})

test('other-source delete raises a delete banner with save/discard options', async ({
  page,
}) => {
  await seed('_e2e/extdel.md', '# Original\n')

  // No /ws/ui delete op exists (backend frozen), so inject the file.delete
  // NOTIFY the server would broadcast, exercising the client's delete handling.
  let liveWs: import('@playwright/test').WebSocketRoute | null = null
  await page.routeWebSocket(/\/ws\/ui$/, (ws) => {
    const server = ws.connectToServer()
    liveWs = ws
    ws.onMessage((m) => server.send(m))
    server.onMessage((m) => ws.send(m))
  })

  await page.goto(editUrl('_e2e/extdel.md'))
  await expect(page.locator('.cm-content')).toBeVisible()

  liveWs!.send(
    JSON.stringify({
      type: 'NOTIFY',
      payload: {
        seq: 99999,
        kind: 'file.delete',
        target: `${NS}/${PROJ}`,
        path: '_e2e/extdel.md',
      },
    }),
  )

  await expect(
    page.getByText('This file was deleted by someone else.'),
  ).toBeVisible()
  await expect(
    page.getByRole('button', { name: 'Save mine as new file' }),
  ).toBeVisible()
})

test('a dirty buffer arms the browser beforeunload prompt; a clean one does not', async ({
  page,
}) => {
  await seed('_e2e/beforeunload.md', '# Original\n')
  await page.goto(editUrl('_e2e/beforeunload.md'))
  await expect(page.locator('.cm-content')).toBeVisible()

  const prevented = () =>
    page.evaluate(() => {
      const e = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(e)
      return e.defaultPrevented
    })

  expect(await prevented()).toBe(false) // clean → no prompt
  await replaceBuffer(page, 'unsaved work')
  expect(await prevented()).toBe(true) // dirty → browser would prompt
})

test('view↔edit toggle via the command palette', async ({ page }) => {
  await seed('_e2e/palette.md', '# Palette\n')
  await page.goto(blobUrl('_e2e/palette.md'))
  await page.keyboard.press('Meta+k')
  const input = page.getByPlaceholder('Type a command or search…')
  await expect(input).toBeVisible()
  await input.fill('Edit current file')
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(new RegExp('/edit/_e2e/palette.md$'))
  await expect(page.locator('.cm-content')).toBeVisible()
})
