import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { Sidebar } from './Sidebar'

// Render the Sidebar at "/" (no project open) inside a minimal memory router so
// its empty-state <Link to="/"> resolves. With no active project the Explorer
// and Search panes show only their empty prompt — no project tree query runs,
// so no QueryClient is needed.
function renderSidebar(view: 'explorer' | 'search') {
  const rootRoute = createRootRoute({
    component: () => <Sidebar view={view} />,
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
  render(<RouterProvider router={router as never} />)
}

describe('Sidebar empty-state terminology (B-31)', () => {
  it('Explorer prompts "Choose a project", not "Choose a repository"', async () => {
    renderSidebar('explorer')
    expect(await screen.findByText('Choose a project →')).toBeInTheDocument()
    expect(screen.queryByText(/repositor/i)).toBeNull()
  })

  it('Search prompts "Choose a project", not "Choose a repository"', async () => {
    renderSidebar('search')
    expect(await screen.findByText('Choose a project →')).toBeInTheDocument()
    expect(screen.queryByText(/repositor/i)).toBeNull()
  })
})

// History panel: with a file open it is the entry point to the history route, not
// the old "lands in a later session" placeholder.
function renderHistorySidebar(url: string) {
  const rootRoute = createRootRoute({
    component: () => <Sidebar view="history" />,
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
  const historyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/history/$',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      indexRoute,
      projectRoute,
      blobRoute,
      historyRoute,
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
}

describe('Sidebar History panel (B-31 phase 2)', () => {
  it('with a file open, links to that file’s history route (no placeholder)', async () => {
    renderHistorySidebar('/p/ns/proj/blob/doc.md')
    const link = await screen.findByRole('link', { name: /view history/i })
    expect(link).toHaveAttribute(
      'href',
      expect.stringContaining('/p/ns/proj/history/doc.md'),
    )
    // The old "lands in a later session" placeholder is gone.
    expect(screen.queryByText(/lands in a later session/i)).toBeNull()
  })

  it('with no file open, prompts to open a file (still no placeholder)', async () => {
    renderHistorySidebar('/p/ns/proj')
    expect(
      await screen.findByText(/open a file to view its commit history/i),
    ).toBeInTheDocument()
    expect(screen.queryByText(/lands in a later session/i)).toBeNull()
  })
})
