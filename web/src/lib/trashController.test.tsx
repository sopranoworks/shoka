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
      {/* Drag-to-trash lifecycle handlers the file-tree row + rail delegate to. */}
      <button onClick={() => t.onRowDragStart({ namespace: 'n', project: 'p', path: 'c.md' })}>
        row-dragstart
      </button>
      <button onClick={() => t.onTrashDragEnter()}>rail-dragenter</button>
      <button onClick={() => t.onTrashDragLeave()}>rail-dragleave</button>
      <button onClick={() => t.onRowDragEnd()}>row-dragend</button>
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

// B-31 fix F (drag-to-trash) — the robust dragend fallback. react-arborist's
// react-dnd HTML5Backend suppresses the rail's native `drop` over a non-dnd target,
// so drag-to-trash must also enqueue from the source row's dragend when the drag was
// released over the trash box (tracked via the rail's dragenter/dragleave). These
// drive the REAL controller handlers the row + rail delegate to (react-arborist rows
// cannot render in jsdom, so the bridge is verified at this seam).
describe('TrashProvider drag-to-trash dragend fallback (B-31 F)', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'c.md' })
    setDragSource(null)
  })

  // RED→GREEN core: a row dropped ON the trash box enqueues even when no native drop
  // fires. RED before the fix: dragend only cleared the source — nothing enqueued.
  it('enqueues a row released over the trash box via dragend (RED→GREEN)', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'c.md', content: 'x', etag: 'e-c' })
    renderHarness()

    fireEvent.click(await screen.findByText('row-dragstart')) // dragSource = c.md
    fireEvent.click(screen.getByText('rail-dragenter')) // drag is over the trash box
    fireEvent.click(screen.getByText('row-dragend')) // released over trash → enqueue

    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )
    expect(readFileFreshSpy).toHaveBeenCalledWith('n', 'p', 'c.md')
    expect(deleteFileSpy).not.toHaveBeenCalled() // deferred, not immediate
  })

  // A drag NOT released over the trash (e.g. an in-tree move drop on a folder) must
  // NOT enqueue on dragend — the over-trash gate prevents collateral deletion.
  it('does NOT enqueue when the drag ends away from the trash box', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'c.md', content: 'x', etag: 'e-c' })
    renderHarness()

    fireEvent.click(await screen.findByText('row-dragstart'))
    // no rail-dragenter → not over the trash
    fireEvent.click(screen.getByText('row-dragend'))

    await Promise.resolve()
    expect(screen.getByTestId('count')).toHaveTextContent('0')
    expect(readFileFreshSpy).not.toHaveBeenCalled()
  })

  // The native drop and the dragend fallback must never double-enqueue the same file:
  // enqueueFromDrag consumes the source, so the second path no-ops.
  it('does not double-enqueue when both the native drop and dragend fire', async () => {
    readFileFreshSpy.mockResolvedValue({ path: 'c.md', content: 'x', etag: 'e-c' })
    renderHarness()

    fireEvent.click(await screen.findByText('row-dragstart'))
    fireEvent.click(screen.getByText('rail-dragenter'))
    fireEvent.click(screen.getByText('drop')) // native drop path consumes the source
    fireEvent.click(screen.getByText('row-dragend')) // fallback finds nothing → no-op

    await waitFor(() =>
      expect(screen.getByTestId('count')).toHaveTextContent('1'),
    )
    expect(readFileFreshSpy).toHaveBeenCalledTimes(1)
  })
})

// B-31 fix H (auto-open / auto-collapse). The rule: auto-open on enqueue; auto-
// collapse the moment the queue empties; the manual toggle is respected while items
// remain.
describe('TrashProvider auto-open / auto-collapse (B-31 H)', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'a.md' })
    setDragSource(null)
  })

  // RED→GREEN core: cancelling the last item auto-collapses the pane. RED before the
  // fix: the pane auto-opened on enqueue but stayed open (empty) after cancelling.
  it('auto-collapses when the queue empties (RED→GREEN)', async () => {
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

  // While items remain, the manual toggle is fully respected (no auto-anything).
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
