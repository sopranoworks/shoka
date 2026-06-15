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
import { setDragSource } from './dragSource'

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

// A tiny consumer exposing the controller's trigger entry points + observable
// state, so we can drive enqueuePath (the right-click Delete… path) and
// enqueueFromDrag (the drag-to-trash drop path) and observe the reservation.
function Harness() {
  const t = useTrashController()
  return (
    <div>
      <button onClick={() => void t.enqueuePath({ namespace: 'n', project: 'p', path: 'a.md' })}>
        delete-a
      </button>
      <button onClick={() => t.enqueueFromDrag()}>drop</button>
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

describe('TrashProvider triggers', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'a.md' })
  })

  // Trigger 1 (tree right-click "Delete…"): enqueuePath captures the file's
  // current etag (the if_match the deferred delete carries) and reserves it,
  // opening the trash pane. It must NOT delete on enqueue.
  it('right-click Delete… (enqueuePath) reserves the file with its current etag and opens the pane', async () => {
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

  // Trigger 2 (drag-to-trash): enqueueFromDrag reads the file recorded at
  // drag-start (lib/dragSource) and reserves it through the SAME path as trigger 1.
  it('drag-to-trash (enqueueFromDrag) enqueues equivalently from the drag source', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'b.md', content: 'x', etag: 'e-b' })
    setDragSource({ namespace: 'n', project: 'p', path: 'b.md' })
    renderHarness()

    fireEvent.click(await screen.findByText('drop'))

    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )
    expect(readFileFreshSpy).toHaveBeenCalledWith('n', 'p', 'b.md')
    expect(deleteFileSpy).not.toHaveBeenCalled()
  })

  it('a drop with no drag source is a no-op (nothing reserved)', async () => {
    setDragSource(null)
    renderHarness()

    fireEvent.click(await screen.findByText('drop'))

    // Give any (absent) async enqueue a tick; the count stays 0.
    await Promise.resolve()
    expect(screen.getByTestId('count')).toHaveTextContent('0')
    expect(readFileFreshSpy).not.toHaveBeenCalled()
  })
})
