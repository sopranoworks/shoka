import { test, expect, type Page } from '@playwright/test'
import { backendCreateProject, backendWrite, backendRead } from './control'

const NS = 'default'
const PROJ = 'upload-dialog-test'

test.beforeAll(async () => {
  await backendCreateProject(NS, PROJ)
  await backendWrite(NS, PROJ, 'README.md', '# Upload dialog test\n')
  await backendWrite(NS, PROJ, 'docs/guide.md', '# Guide\n')
})

async function openProject(page: Page) {
  await page.goto(`/p/${NS}/${PROJ}/blob/README.md`)
  await expect(page.getByTestId('upload-btn')).toBeVisible({ timeout: 10_000 })
}

async function openDialog(page: Page) {
  await page.getByTestId('upload-btn').click()
  await expect(page.getByTestId('upload-dialog')).toBeVisible()
}

function mdFile(name: string, content: string) {
  return { name, mimeType: 'text/markdown', buffer: Buffer.from(content) }
}

function csvFile(name: string, content: string) {
  return { name, mimeType: 'text/csv', buffer: Buffer.from(content) }
}

test('Upload .md file to existing directory', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await page.getByTestId('upload-file-input').setInputFiles(
    mdFile('uploaded.md', '# Uploaded via dialog\n'),
  )

  await expect(page.getByTestId('upload-file-list')).toBeVisible()
  await expect(page.getByTestId('upload-file-list').getByText('uploaded.md')).toBeVisible()

  await page.getByTestId('upload-dir-select').selectOption('docs')

  await page.getByTestId('upload-confirm').click()
  await expect(page.getByTestId('upload-dialog')).not.toBeVisible({ timeout: 10_000 })

  const content = await backendRead(NS, PROJ, 'docs/uploaded.md')
  expect(content).toBe('# Uploaded via dialog\n')

  await page.locator('#sidebar').getByText('docs', { exact: true }).click()
  await expect(
    page.locator('#sidebar').getByText('uploaded.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })
})

test('Upload to new directory', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await page.getByTestId('upload-file-input').setInputFiles(
    mdFile('newdir-file.md', '# In new dir\n'),
  )

  await page.getByTestId('upload-new-path').fill('test-upload/sub')
  await expect(page.getByTestId('upload-path-error')).not.toBeVisible()

  await page.getByTestId('upload-confirm').click()
  await expect(page.getByTestId('upload-dialog')).not.toBeVisible({ timeout: 10_000 })

  const content = await backendRead(NS, PROJ, 'test-upload/sub/newdir-file.md')
  expect(content).toBe('# In new dir\n')

  await page.locator('#sidebar').getByText('test-upload', { exact: true }).click()
  await expect(
    page.locator('#sidebar').getByText('sub', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })
  await page.locator('#sidebar').getByText('sub', { exact: true }).click()
  await expect(
    page.locator('#sidebar').getByText('newdir-file.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })
})

test('Upload CSV with conversion', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await page.getByTestId('upload-file-input').setInputFiles(
    csvFile('data.csv', 'Name,Age,City\nAlice,30,Tokyo\nBob,25,Osaka\n'),
  )

  await expect(page.getByTestId('upload-file-list').getByText('data.csv')).toBeVisible()
  await expect(
    page.getByTestId('upload-file-list').getByText(/will be converted to data\.md/),
  ).toBeVisible()

  await page.getByTestId('upload-confirm').click()
  await expect(page.getByTestId('upload-dialog')).not.toBeVisible({ timeout: 10_000 })

  await expect(
    page.locator('#sidebar').getByText('data.md', { exact: true }),
  ).toBeVisible({ timeout: 10_000 })

  const content = await backendRead(NS, PROJ, 'data.md')
  expect(content).toContain('| Name | Age | City |')
  expect(content).toContain('| Alice | 30 | Tokyo |')
})

test('Invalid path shows red border', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await page.getByTestId('upload-file-input').setInputFiles(
    mdFile('valid.md', '# Valid\n'),
  )

  const pathInput = page.getByTestId('upload-new-path')
  await pathInput.fill('../bad')
  await expect(page.getByTestId('upload-path-error')).toBeVisible()
  await expect(page.getByTestId('upload-confirm')).toBeDisabled()

  await pathInput.fill('valid/path')
  await expect(page.getByTestId('upload-path-error')).not.toBeVisible()
  await expect(page.getByTestId('upload-confirm')).toBeEnabled()
})

test('Upload button disabled without file', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await expect(page.getByTestId('upload-confirm')).toBeDisabled()

  await page.getByTestId('upload-file-input').setInputFiles(
    mdFile('enable-test.md', '# Enable\n'),
  )

  await expect(page.getByTestId('upload-confirm')).toBeEnabled()
})

test('Cancel closes dialog', async ({ page }) => {
  await openProject(page)
  await openDialog(page)

  await page.getByTestId('upload-file-input').setInputFiles(
    mdFile('cancel-test.md', '# Cancel\n'),
  )
  await expect(page.getByTestId('upload-file-list').getByText('cancel-test.md')).toBeVisible()

  await page.getByTestId('upload-dialog').getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(page.getByTestId('upload-dialog')).not.toBeVisible()

  await expect(
    page.locator('#sidebar').getByText('cancel-test.md', { exact: true }),
  ).toHaveCount(0)
})
