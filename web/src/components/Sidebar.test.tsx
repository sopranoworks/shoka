import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import {
  Sidebar,
  ShellProvider,
  ContentProvider,
  useSimpleRailControls,
  useNoopRailReset,
} from '@shoka/web-core'

const minShell = {
  railItems: [],
  renderSidebar: () => null,
  useRailControls: useSimpleRailControls,
  useResetRailOnProjectChange: useNoopRailReset,
}

function renderSidebar(view: 'explorer' | 'search') {
  const rootRoute = createRootRoute({
    component: () => (
      <ShellProvider value={minShell}>
        <ContentProvider>
          <Sidebar view={view} />
        </ContentProvider>
      </ShellProvider>
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
  render(<RouterProvider router={router as never} />)
}

describe('Sidebar Explorer pane with no project open (B-31 C)', () => {
  it('renders no EXPLORER heading and no "Choose a project" cushion', () => {
    renderSidebar('explorer')
    expect(screen.queryByText('Choose a project →')).toBeNull()
    expect(screen.queryByText(/^explorer$/i)).toBeNull()
    expect(screen.queryByText(/repositor/i)).toBeNull()
  })
})
