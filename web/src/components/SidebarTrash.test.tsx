import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { ToastProvider } from '../lib/toast'
import { TrashProvider, useTrashController } from '../lib/trashController'
import { SidebarTrash } from './SidebarTrash'

// B-31 fix G: the trash pane must be an IN-COLUMN collapsible section beneath the
// file tree (mounted by SidebarTrash inside the sidebar column), NOT a floating
// dialog/overflow popover rendered by TrashProvider over the whole shell. These
// assert the STRUCTURE (not pixels): where the pane mounts, and that the provider no
// longer floats it.

const readFileFreshSpy = vi.fn()
vi.mock('../lib/fileOps', () => ({
  readFileFresh: (...args: unknown[]) => readFileFreshSpy(...args),
  deleteFile: vi.fn(),
}))

function EnqueueButton() {
  const t = useTrashController()
  return (
    <button onClick={() => void t.enqueuePath({ namespace: 'n', project: 'p', path: 'a.md' })}>
      enqueue
    </button>
  )
}

// Renders `body` under the providers TrashProvider needs (router/query/toast).
function renderUnderProviders(body: ReactNode) {
  const rootRoute = createRootRoute({
    component: () => <TrashProvider graceMs={10_000}>{body}</TrashProvider>,
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

describe('trash pane is in-column, not a floating dialog (B-31 G)', () => {
  beforeEach(() => {
    readFileFreshSpy.mockReset()
    readFileFreshSpy.mockResolvedValue({ path: 'a.md', content: 'x', etag: 'cur' })
  })

  it('SidebarTrash mounts the pane inside the sidebar column, beneath the tree', async () => {
    renderUnderProviders(
      <div data-testid="sidebar-col">
        <div data-testid="tree">file tree</div>
        <SidebarTrash />
        <EnqueueButton />
      </div>,
    )

    // Closed by default: no pane mounted.
    const enqueue = await screen.findByText('enqueue')
    expect(screen.queryByRole('region', { name: /trash/i })).toBeNull()

    fireEvent.click(enqueue)
    const pane = await screen.findByRole('region', { name: /trash/i })

    // The pane lives INSIDE the sidebar column (in-column section), as a sibling
    // AFTER the tree — not a floating overlay elsewhere in the document.
    const col = screen.getByTestId('sidebar-col')
    expect(col).toContainElement(pane)
    expect(within(col).getByTestId('tree').compareDocumentPosition(pane)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    )
    // It is a region/section, never a modal dialog.
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('TrashProvider no longer renders the pane itself (the float was removed)', async () => {
    // Provider with children but WITHOUT SidebarTrash: enqueue, and assert no pane
    // appears anywhere — the provider must not float it. RED before the fix: the
    // provider rendered the position:fixed pane here.
    renderUnderProviders(<EnqueueButton />)

    fireEvent.click(await screen.findByText('enqueue'))
    await waitFor(() => expect(readFileFreshSpy).toHaveBeenCalled())
    // Give the enqueue microtasks a tick, then assert still no pane.
    await Promise.resolve()
    expect(screen.queryByRole('region', { name: /trash/i })).toBeNull()
  })
})
