import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { ActivityRail, type RailView } from '../components/ActivityRail'
import { useRailSelect } from './useRailSelect'

// Render the ActivityRail wired to the real useRailSelect handler inside a memory
// router at `url`. The rail is pure, so no QueryClient/wsClient is needed (the
// full Shell would drag them in) — this isolates the routing decision.
function setup(url: string, setRail: (v: RailView) => void = () => {}) {
  function Harness() {
    const onSelect = useRailSelect(setRail, () => {})
    return <ActivityRail active="explorer" onSelect={onSelect} />
  }
  const rootRoute = createRootRoute({ component: Harness })
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
  const adminRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/admin/connections',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, projectRoute, adminRoute]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

// Fix #1: on an admin screen (no project in context) the Explorer/Search/History
// rail items were a silent no-op (they only set local pane state, which has
// nothing to show there). They must route to "/" instead. RED before the fix:
// clicking changed neither the URL nor anything visible.
describe('useRailSelect — admin/no-project routes to "/" (B-31 #1)', () => {
  it.each(['Explorer', 'Search', 'History'])(
    'clicking %s on /admin/connections navigates to "/"',
    async (label) => {
      const router = setup('/admin/connections')
      expect(router.state.location.pathname).toBe('/admin/connections')
      fireEvent.click(await screen.findByRole('button', { name: label }))
      await waitFor(() =>
        expect(router.state.location.pathname).toBe('/'),
      )
    },
  )
})

// Fix #1 must NOT regress the normal project view: there the rail switches the
// sidebar pane (Explorer/Search/History) and does not navigate away.
describe('useRailSelect — project view keeps the pane behaviour (no regression)', () => {
  it('clicking Explorer in a project view sets the rail and does not navigate', async () => {
    const setRail = vi.fn()
    const router = setup('/p/ns/proj', setRail)
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    expect(setRail).toHaveBeenCalledWith('explorer')
    // Give any (unexpected) navigation a chance to settle, then assert we stayed.
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/p/ns/proj'),
    )
  })

  it('clicking History in a project view sets the rail to history', async () => {
    const setRail = vi.fn()
    setup('/p/ns/proj', setRail)
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    expect(setRail).toHaveBeenCalledWith('history')
  })
})
