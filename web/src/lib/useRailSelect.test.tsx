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

// Render the ActivityRail wired to the real useRailSelect controls (onSelect +
// disabledItems) inside a memory router at `url` — mirroring exactly how Shell
// composes them. The rail is pure, so no QueryClient/wsClient is needed.
function setup(url: string, setRail: (v: RailView) => void = () => {}) {
  function Harness() {
    const { onSelect, disabledItems } = useRailSelect(setRail, () => {})
    return (
      <ActivityRail active="explorer" onSelect={onSelect} disabled={disabledItems} />
    )
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
      adminRoute,
      blobRoute,
      historyRoute,
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

// Fix (B-31 refinement): on an admin/no-project route, Explorer still routes to
// "/" (return to where the files are), but Search and History are DISABLED — they
// have no meaningful action there (OAuth-token search/history is unbuilt), so they
// must be inert, not active-looking buttons that route to "/".
describe('useRailSelect — admin: Explorer→"/", Search/History disabled', () => {
  it('Explorer on /admin/connections navigates to "/"', async () => {
    const router = setup('/admin/connections')
    expect(router.state.location.pathname).toBe('/admin/connections')
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/'))
  })

  it.each(['Search', 'History'])(
    '%s on /admin/connections is disabled and does nothing on click (RED→GREEN)',
    async (label) => {
      const router = setup('/admin/connections')
      const btn = await screen.findByRole('button', { name: label })
      expect(btn).toBeDisabled()
      expect(btn).toHaveAttribute('aria-disabled', 'true')
      // Not active-highlighted even if it was the last-selected pane.
      expect(btn).toHaveAttribute('data-active', 'false')
      // Clicking a disabled button must not navigate (RED before: it went to "/").
      fireEvent.click(btn)
      await waitFor(() =>
        expect(router.state.location.pathname).toBe('/admin/connections'),
      )
    },
  )
})

// No regression in a normal project view: all three rail items stay enabled and
// switch the sidebar pane (no navigation).
describe('useRailSelect — project view: all three enabled (no regression)', () => {
  it.each(['Explorer', 'Search', 'History'])(
    '%s in a project view is enabled',
    async (label) => {
      setup('/p/ns/proj')
      const btn = await screen.findByRole('button', { name: label })
      expect(btn).toBeEnabled()
      expect(btn).toHaveAttribute('aria-disabled', 'false')
    },
  )

  it('clicking Explorer in a project view sets the rail and does not navigate', async () => {
    const setRail = vi.fn()
    const router = setup('/p/ns/proj', setRail)
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    expect(setRail).toHaveBeenCalledWith('explorer')
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

// Fix A: clicking History opens the active file's history directly in the right
// pane (no "View history →" cushion). RED before: History had no navigation — it
// only switched the sidebar to a cushion you had to click again.
describe('useRailSelect — History opens the active file’s history directly', () => {
  it('from a file view, navigates to that file’s history route', async () => {
    const router = setup('/p/ns/proj/blob/doc.md')
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/p/ns/proj/history/doc.md'),
    )
  })

  it('from a nested file view, carries the full path', async () => {
    const router = setup('/p/ns/proj/blob/reports/doc.md')
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        '/p/ns/proj/history/reports/doc.md',
      ),
    )
  })

  it('with no file selected (project root), opens the history route empty (placeholder pane)', async () => {
    const router = setup('/p/ns/proj')
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toContain('/p/ns/proj/history'),
    )
  })
})
