import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'

// Stub the tree query (non-empty so ProjectTree renders the tree) and the
// FileTree itself (react-arborist needs ResizeObserver, unavailable in jsdom) —
// the stub surfaces the props we care about: openMode and the highlighted path.
vi.mock('../lib/queries', () => ({
  useTreeQuery: () => ({
    data: [{ name: 'doc.md', path: 'doc.md', isFile: true }],
    isError: false,
  }),
}))
vi.mock('./FileTree', () => ({
  FileTree: ({
    openMode,
    activePath,
  }: {
    openMode?: string
    activePath?: string | null
  }) => (
    <div
      data-testid="filetree"
      data-openmode={openMode}
      data-active={activePath ?? ''}
    />
  ),
}))

import { Sidebar } from './Sidebar'

function renderSidebar(view: 'explorer' | 'history', url: string) {
  const rootRoute = createRootRoute({
    component: () => <Sidebar view={view} />,
  })
  const mk = (path: string) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: () => null })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      mk('/'),
      mk('/p/$namespace/$project'),
      mk('/p/$namespace/$project/blob/$'),
      mk('/p/$namespace/$project/history/$'),
      mk('/p/$namespace/$project/new'),
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

// Fix A: the History rail keeps the file tree in place and shows history in the
// right pane — the old "View history →" cushion is gone.
describe('Sidebar History panel — tree retained, no cushion (B-31)', () => {
  it('renders the file tree (openMode="history") and NO "View history →" cushion', async () => {
    renderSidebar('history', '/p/ns/proj/history/doc.md')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'history')
    // The history tree highlights the file the right pane is showing.
    expect(tree).toHaveAttribute('data-active', 'doc.md')
    // The pointless cushion link is gone.
    expect(screen.queryByText(/view history/i)).toBeNull()
  })

  it('with no file selected, still shows the tree (placeholder lives in the right pane)', async () => {
    renderSidebar('history', '/p/ns/proj')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'history')
    expect(tree).toHaveAttribute('data-active', '')
    expect(screen.queryByText(/view history/i)).toBeNull()
  })

  it('Explorer mode renders the same tree but opens files (openMode="blob")', async () => {
    renderSidebar('explorer', '/p/ns/proj/blob/doc.md')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'blob')
    expect(tree).toHaveAttribute('data-active', 'doc.md')
  })
})

// E (B-31 consistency fix): the "+ New file" affordance is present (clearly
// visible at rest) in Explorer mode, but hidden in History mode — creating a file
// from a history view is meaningless.
describe('Sidebar "+ New file" affordance — Explorer only (B-31 E)', () => {
  it('is present at rest in Explorer mode (its resting class, no hover needed)', async () => {
    renderSidebar('explorer', '/p/ns/proj/blob/doc.md')
    const btn = await screen.findByRole('button', { name: 'New file' })
    // Rendered with its (resting) class — visibility is not gated behind a hover
    // pseudo-class; the resting style itself is legible (E1, a CSS change).
    expect(btn.className).toMatch(/newFileBtn/)
  })

  it('is NOT rendered in History mode (RED→GREEN)', async () => {
    renderSidebar('history', '/p/ns/proj/history/doc.md')
    await screen.findByTestId('filetree')
    expect(screen.queryByRole('button', { name: 'New file' })).toBeNull()
  })
})
