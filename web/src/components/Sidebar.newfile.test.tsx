import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'

// The Explorer tree query is irrelevant to the header affordance under test;
// stub it to an empty tree so ExplorerForProject renders its header (and the
// "+ New file" button) without a QueryClient.
vi.mock('../lib/queries', () => ({
  useTreeQuery: () => ({ data: [], isError: false }),
}))

import { Sidebar } from './Sidebar'

// Mount the Explorer sidebar at `url` in a memory router that includes the /new
// route (with the ?in= search), so clicking "+ New file" resolves a real
// navigation we can assert on.
function renderExplorer(url: string) {
  const rootRoute = createRootRoute({
    component: () => <Sidebar view="explorer" />,
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const projectRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project',
    component: () => null,
  })
  const blobRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/blob/$',
    component: () => null,
  })
  const newFileRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/new',
    validateSearch: (s: Record<string, unknown>) =>
      typeof s.in === 'string' && s.in ? { in: s.in } : {},
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      indexRoute,
      projectRoute,
      blobRoute,
      newFileRoute,
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

// Fix #3: the create flow must be reachable from the Explorer header, prefilled
// with the current location.
describe('Explorer "+ New file" affordance (B-31 #3)', () => {
  it('from a file view, navigates to /new carrying the file’s directory (in=subdir)', async () => {
    const router = renderExplorer('/p/ns/proj/blob/subdir/note.md')
    fireEvent.click(await screen.findByRole('button', { name: 'New file' }))
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/p/ns/proj/new')
      expect(router.state.location.search).toEqual({ in: 'subdir' })
    })
  })

  it('from the project root, navigates to /new with no prefill', async () => {
    const router = renderExplorer('/p/ns/proj')
    fireEvent.click(await screen.findByRole('button', { name: 'New file' }))
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/p/ns/proj/new')
      expect(router.state.location.search).toEqual({})
    })
  })
})
