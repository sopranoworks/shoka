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

// The History-panel behaviour (B-31 fix: tree retained, no "View history →"
// cushion) is exercised in Sidebar.history.test.tsx, which mocks the tree query.
