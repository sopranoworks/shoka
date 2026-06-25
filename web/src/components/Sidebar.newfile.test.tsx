import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
  Link,
} from '@tanstack/react-router'

vi.mock('../../../packages/web-core/src/lib/queries', async (importOriginal) => {
  const orig = await importOriginal<Record<string, unknown>>()
  return { ...orig, useTreeQuery: () => ({ data: [], isError: false }) }
})

import {
  Sidebar,
  ShellProvider,
  ContentProvider,
  useSimpleRailControls,
  useNoopRailReset,
  sidebarStyles,
  dirOf,
} from '@shoka/web-core'

const minShell = {
  railItems: [],
  renderSidebar: () => null,
  useRailControls: useSimpleRailControls,
  useResetRailOnProjectChange: useNoopRailReset,
}

function renderExplorer(url: string) {
  const rootRoute = createRootRoute({
    component: () => (
      <ShellProvider value={minShell}>
        <ContentProvider
          value={{
            renderNewFileButton: (ns, proj, launchDir) => (
              <Link
                to="/p/$namespace/$project/new"
                params={{ namespace: ns, project: proj }}
                search={launchDir ? { in: launchDir } : {}}
                className={sidebarStyles.newFileBtn}
              >
                + New file
              </Link>
            ),
          }}
        >
          <Sidebar view="explorer" />
        </ContentProvider>
      </ShellProvider>
    ),
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

describe('Explorer "+ New file" affordance (B-31 #3)', () => {
  it('from a file view, navigates to /new carrying the file’s directory (in=subdir)', async () => {
    const router = renderExplorer('/p/ns/proj/blob/subdir/note.md')
    fireEvent.click(await screen.findByRole('link', { name: '+ New file' }))
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/p/ns/proj/new')
      expect(router.state.location.search).toEqual({ in: 'subdir' })
    })
  })

  it('from the project root, navigates to /new with no prefill', async () => {
    const router = renderExplorer('/p/ns/proj')
    fireEvent.click(await screen.findByRole('link', { name: '+ New file' }))
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/p/ns/proj/new')
      expect(router.state.location.search).toEqual({})
    })
  })
})
