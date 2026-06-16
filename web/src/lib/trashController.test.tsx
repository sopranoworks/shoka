import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { ToastProvider } from './toast'
import { TrashProvider, useTrashController } from './trashController'

// Mock fileOps: readFileFresh captures the enqueue-time etag; deleteFile is the
// deferred write (not exercised here — the queue's timing is covered in
// trashQueue.test.ts). Deref is deferred (arrow body) so the spies are read at
// call time (mirrors fileOps.test.ts's wsClient mock).
const readFileFreshSpy = vi.fn()
const deleteFileSpy = vi.fn()
vi.mock('./fileOps', () => ({
  readFileFresh: (...args: unknown[]) => readFileFreshSpy(...args),
  deleteFile: (...args: unknown[]) => deleteFileSpy(...args),
}))

// A tiny consumer exposing the controller's enqueue entry point + pane state.
// (Drag-to-trash is now a react-dnd drop delivered by Shell's ShellRail, proven by
// the real-browser E2E tests/e2e/trash-dnd.spec.ts — not a controller-level seam.)
function Harness() {
  const t = useTrashController()
  return (
    <div>
      <button onClick={() => void t.enqueuePath({ namespace: 'n', project: 'p', path: 'a.md' })}>
        delete-a
      </button>
      <button onClick={() => t.togglePane()}>toggle</button>
      <button onClick={() => t.items.forEach((i) => t.cancel(i.id))}>cancel-all</button>
      <span data-testid="count">{t.items.length}</span>
      <span data-testid="pane">{t.paneOpen ? 'open' : 'closed'}</span>
    </div>
  )
}

function renderHarness() {
  // TrashProvider needs the router (useNavigate/useRouterState), so it lives
  // inside RouterProvider; QueryClient + Toast wrap the router (as in main.tsx).
  const rootRoute = createRootRoute({
    component: () => (
      <TrashProvider graceMs={10_000}>
        <Harness />
      </TrashProvider>
    ),
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  const queryClient = new QueryClient()
  render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <RouterProvider router={router as never} />
      </ToastProvider>
    </QueryClientProvider>,
  )
}

describe('TrashProvider enqueue (retained unit coverage; NOT the proof of drag-to-trash)', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'a.md' })
  })

  // enqueuePath is the SINGLE delete-reservation path both triggers funnel through
  // (right-click Delete… and the react-dnd trash drop): it captures the file's
  // current etag (the if_match the deferred delete carries) and reserves it, opening
  // the pane. It must NOT delete on enqueue.
  it('enqueuePath reserves the file with its current etag and opens the pane', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'a.md', content: 'x', etag: 'cur-etag' })
    renderHarness()

    fireEvent.click(await screen.findByText('delete-a'))

    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )
    expect(screen.getByTestId('pane')).toHaveTextContent('open')
    expect(readFileFreshSpy).toHaveBeenCalledWith('n', 'p', 'a.md')
    expect(deleteFileSpy).not.toHaveBeenCalled() // deferred, not immediate
  })
})

// B-31 fix H (auto-open / auto-collapse). The rule: auto-open on enqueue; auto-
// collapse the moment the queue empties; the manual toggle is respected while items
// remain. (Kept from 5be545d; not reported broken — re-verified here.)
describe('TrashProvider auto-open / auto-collapse (B-31 H)', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'a.md' })
  })

  it('auto-collapses when the queue empties', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'a.md', content: 'x', etag: 'cur' })
    renderHarness()

    fireEvent.click(await screen.findByText('delete-a'))
    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )
    expect(screen.getByTestId('pane')).toHaveTextContent('open') // auto-opened

    fireEvent.click(screen.getByText('cancel-all'))
    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('0'),
    )
    await waitFor(() =>
      expect(screen.getByTestId('pane')).toHaveTextContent('closed'),
    ) // auto-collapsed
  })

  it('respects the manual toggle while items remain', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'a.md', content: 'x', etag: 'cur' })
    renderHarness()

    fireEvent.click(await screen.findByText('delete-a'))
    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )

    fireEvent.click(screen.getByText('toggle')) // manual close with an item present
    expect(screen.getByTestId('pane')).toHaveTextContent('closed')
    fireEvent.click(screen.getByText('toggle')) // manual reopen
    expect(screen.getByTestId('pane')).toHaveTextContent('open')
    expect(screen.getByTestId('count')).toHaveTextContent('1') // item still queued
  })
})
