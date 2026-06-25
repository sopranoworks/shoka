import { render, screen, fireEvent, within, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ThemeProvider } from '@shoka/web-core'

// The create flow's two collaborators, stubbed so we assert the UI behaviour (the
// prefill, the full path the create carries) rather than the /ws/ui path.
const saveFile = vi.fn()
const fileExists = vi.fn()
vi.mock('../lib/fileOps', () => ({
  saveFile: (...a: unknown[]) => saveFile(...a),
  fileExists: (...a: unknown[]) => fileExists(...a),
}))

// CodeMirror is heavy and not under test here; replace it with a plain textarea
// that forwards onChange so the buffer can be made dirty (which enables Save…).
vi.mock('@uiw/react-codemirror', () => ({
  default: ({
    value,
    onChange,
  }: {
    value: string
    onChange: (v: string) => void
  }) => (
    <textarea
      data-testid="cm"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  ),
}))

import { NewFilePage } from './NewFilePage'

// Mount NewFilePage at `url` inside a memory router whose /new route mirrors the
// real route's id + ?in= search (so NewFilePage's route hooks resolve), with a
// blob route for the post-save redirect.
function renderNew(url: string) {
  const qc = new QueryClient()
  const rootRoute = createRootRoute()
  const newFileRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/new',
    validateSearch: (s: Record<string, unknown>) =>
      typeof s.in === 'string' && s.in ? { in: s.in } : {},
    component: NewFilePage,
  })
  const blobRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/blob/$',
    component: () => <div>blob</div>,
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([newFileRoute, blobRoute, indexRoute]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <RouterProvider router={router as never} />
      </ThemeProvider>
    </QueryClientProvider>,
  )
  return router
}

// Open the Save-path dialog: typing makes the buffer dirty, which enables Save….
async function openSaveDialog() {
  fireEvent.change(await screen.findByTestId('cm'), {
    target: { value: 'hello' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Save…' }))
  return within(screen.getByRole('dialog')).getByRole('textbox') as HTMLInputElement
}

beforeEach(() => {
  saveFile.mockReset()
  fileExists.mockReset()
})

// Fix #3: the dialog is prefilled with the current location.
describe('NewFilePage path prefill (B-31 #3)', () => {
  it('prefills the directory of the file it was launched from (in=subdir → "subdir/")', async () => {
    renderNew('/p/ns/proj/new?in=subdir')
    const input = await openSaveDialog()
    expect(input.value).toBe('subdir/')
  })

  it('prefills empty at the project root (no in)', async () => {
    renderNew('/p/ns/proj/new')
    const input = await openSaveDialog()
    expect(input.value).toBe('')
  })
})

// Fix #4: an edited nested path creates the file there — the create call carries
// the full path (the server makes the intermediate dirs; that side is its own
// tested concern).
describe('NewFilePage nested-path create (B-31 #4)', () => {
  it('SAVE_FILE carries the full nested path the user typed', async () => {
    fileExists.mockResolvedValue({ exists: false })
    saveFile.mockResolvedValue({ ok: true, path: 'a/b/c.md', etag: 'e1' })

    renderNew('/p/ns/proj/new')
    const input = await openSaveDialog()
    fireEvent.change(input, { target: { value: 'a/b/c.md' } })
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Save' }),
    )

    await waitFor(() =>
      expect(saveFile).toHaveBeenCalledWith(
        expect.objectContaining({ path: 'a/b/c.md', ifMatch: null }),
      ),
    )
  })
})
