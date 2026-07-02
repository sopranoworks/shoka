import { test, expect, type Page } from '@playwright/test'
import { backendCreateProject, backendWrite, backendRead } from './control'
import { dropFile } from './dndHelpers'

const NS = 'default'
const PROJ = 'file-convert-test'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'README.md', '# Convert test\n')
})

async function openProject(page: Page) {
  await page.goto(`/p/${NS}/${PROJ}/blob/README.md`)
  await expect(page.getByTestId('file-dropzone')).toBeVisible()
}

test('CSV upload: confirmation modal → convert and upload → Markdown table', async ({
  page,
}) => {
  await openProject(page)

  await dropFile(page, 'data.csv', 'Name,Age,City\nAlice,30,Tokyo\nBob,25,Osaka\n')

  const dialog = page.getByTestId('conversion-dialog')
  await expect(dialog).toBeVisible({ timeout: 5000 })
  await expect(dialog.getByText('File Conversion Required')).toBeVisible()
  await expect(dialog.getByText(/data\.csv/)).toBeVisible()
  await expect(dialog.getByText(/data\.md/)).toBeVisible()
  await expect(dialog.getByText(/CSV files are converted to Markdown tables/)).toBeVisible()

  await dialog.getByRole('button', { name: 'Convert and Upload' }).click()

  await expect(dialog).not.toBeVisible({ timeout: 5000 })
  await expect(page.getByText('Added data.md')).toBeVisible({ timeout: 10_000 })
  await expect(
    page.locator('#sidebar').getByText('data.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })

  const content = await backendRead(NS, PROJ, 'data.md')
  expect(content).toContain('| Name | Age | City |')
  expect(content).toContain('| Alice | 30 | Tokyo |')
  expect(content).toContain('| Bob | 25 | Osaka |')
})

test('TXT upload: confirmation modal → convert and upload → content preserved', async ({
  page,
}) => {
  await openProject(page)

  await dropFile(page, 'notes.txt', 'Plain text content\nSecond line\n')

  const dialog = page.getByTestId('conversion-dialog')
  await expect(dialog).toBeVisible({ timeout: 5000 })
  await expect(dialog.getByText(/notes\.txt/)).toBeVisible()
  await expect(dialog.getByText(/notes\.md/)).toBeVisible()

  await dialog.getByRole('button', { name: 'Convert and Upload' }).click()

  await expect(dialog).not.toBeVisible({ timeout: 5000 })
  await expect(page.getByText('Added notes.md')).toBeVisible({ timeout: 10_000 })

  const content = await backendRead(NS, PROJ, 'notes.md')
  expect(content).toBe('Plain text content\nSecond line\n')
})

test('Cancel conversion: modal dismissed → file NOT uploaded', async ({ page }) => {
  await openProject(page)

  await dropFile(page, 'cancel-me.csv', 'A,B\n1,2\n')

  const dialog = page.getByTestId('conversion-dialog')
  await expect(dialog).toBeVisible({ timeout: 5000 })

  await dialog.getByRole('button', { name: 'Cancel' }).click()

  await expect(dialog).not.toBeVisible({ timeout: 5000 })
  await expect(
    page.locator('#sidebar').getByText('cancel-me.md', { exact: true }),
  ).toHaveCount(0)
  await expect(
    page.locator('#sidebar').getByText('cancel-me.csv', { exact: true }),
  ).toHaveCount(0)
})

test('Non-UTF-8 file: error toast shown, no modal, no upload', async ({ page }) => {
  await openProject(page)

  await page.evaluate(() => {
    const target = document.querySelector('[data-testid="file-dropzone"]')
    if (!target) throw new Error('no dropzone')
    const r = target.getBoundingClientRect()
    const point = { x: r.x + r.width / 2, y: r.bottom - 6 }

    const dt = new DataTransfer()
    const bytes = new Uint8Array([0xff, 0xfe, 0x80, 0x81, 0x82])
    dt.items.add(new File([bytes], 'bad.csv', { type: 'text/csv' }))
    const opts = {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
      clientX: point.x,
      clientY: point.y,
    }
    target.dispatchEvent(new DragEvent('dragenter', opts))
    target.dispatchEvent(new DragEvent('dragover', opts))
    target.dispatchEvent(new DragEvent('drop', opts))
  })

  await expect(page.getByText(/UTF-8/)).toBeVisible({ timeout: 5000 })
  await expect(page.getByTestId('conversion-dialog')).not.toBeVisible()
  await expect(
    page.locator('#sidebar').getByText('bad.md', { exact: true }),
  ).toHaveCount(0)
  await expect(
    page.locator('#sidebar').getByText('bad.csv', { exact: true }),
  ).toHaveCount(0)
})

test('Normal .md upload: no conversion modal, direct upload', async ({ page }) => {
  await openProject(page)

  await dropFile(page, 'direct.md', '# Direct upload\n')

  await expect(page.getByTestId('conversion-dialog')).not.toBeVisible()
  await expect(page.getByText('Added direct.md')).toBeVisible({ timeout: 10_000 })
  await expect(
    page.locator('#sidebar').getByText('direct.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })
})
