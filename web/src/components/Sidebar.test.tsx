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

// C (B-31 consistency fix): with no project open there is nothing to explore, so
// the Explorer pane renders genuinely empty — no "EXPLORER" heading, no "Choose a
// project →" cushion. (RED before: the cushion was present.)
describe('Sidebar Explorer pane with no project open (B-31 C)', () => {
  it('renders no EXPLORER heading and no "Choose a project" cushion', () => {
    renderSidebar('explorer')
    expect(screen.queryByText('Choose a project →')).toBeNull()
    expect(screen.queryByText(/^explorer$/i)).toBeNull()
    expect(screen.queryByText(/repositor/i)).toBeNull()
  })
})

// The History-panel behaviour (B-31 fix: tree retained, no "View history →"
// cushion) is exercised in Sidebar.history.test.tsx, which mocks the tree query.
