import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import {
  ActivityRail,
  ShellProvider,
  useNoopRailReset,
  type ShellConfig,
} from '@shoka/web-core'
import {
  type RailView,
  useRailSelect,
  useResetRailToExplorerOnProjectChange,
} from './useRailSelect'

const TEST_RAIL_ITEMS: ShellConfig['railItems'] = [
  { id: 'explorer', label: 'Explorer', icon: <span>E</span> },
  { id: 'search', label: 'Search', icon: <span>S</span> },
  { id: 'history', label: 'History', icon: <span>H</span> },
  { id: 'settings', label: 'Settings', icon: <span>G</span> },
]

function testShellConfig(
  overrides: Partial<ShellConfig> = {},
): ShellConfig {
  return {
    railItems: TEST_RAIL_ITEMS,
    renderSidebar: () => null,
    useRailControls: useRailSelect,
    useResetRailOnProjectChange: useNoopRailReset,
    ...overrides,
  }
}

function setup(
  url: string,
  opts: {
    rail?: RailView
    sidebarOpen?: boolean
    setRail?: (v: RailView) => void
    setSidebarOpen?: (open: boolean) => void
  } = {},
) {
  const rail = opts.rail ?? 'explorer'
  const sidebarOpen = opts.sidebarOpen ?? true
  const setRail = opts.setRail ?? (() => {})
  const setSidebarOpen = opts.setSidebarOpen ?? (() => {})
  function Harness() {
    const { onSelect, disabledItems } = useRailSelect(
      rail,
      sidebarOpen,
      setRail,
      setSidebarOpen,
    )
    return (
      <ActivityRail active={rail} onSelect={onSelect} disabled={disabledItems} />
    )
  }
  const rootRoute = createRootRoute({
    component: () => (
      <ShellProvider value={testShellConfig()}>
        <Harness />
      </ShellProvider>
    ),
  })
  const mk = (path: string) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: () => null })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      mk('/'),
      mk('/p/$namespace/$project'),
      mk('/admin/connections'),
      mk('/p/$namespace/$project/blob/$'),
      mk('/p/$namespace/$project/history/$'),
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

describe('useRailSelect — no-project route disables ALL rail items', () => {
  it.each([
    ['/', 'project list'],
    ['/admin/connections', 'admin screen'],
  ])('all three rail items are disabled on %s (%s)', async (url) => {
    const setRail = vi.fn()
    const router = setup(url, { setRail })
    for (const label of ['Explorer', 'Search', 'History']) {
      const btn = await screen.findByRole('button', { name: label })
      expect(btn).toBeDisabled()
      expect(btn).toHaveAttribute('aria-disabled', 'true')
      fireEvent.click(btn)
    }
    expect(router.state.location.pathname).toBe(url)
    expect(setRail).not.toHaveBeenCalled()
  })
})

describe('useRailSelect — consistent open/close toggle (project view)', () => {
  it.each(['explorer', 'search', 'history'] as RailView[])(
    'clicking the active+open %s closes the sidebar (does not re-set the rail)',
    async (active) => {
      const setRail = vi.fn()
      const setSidebarOpen = vi.fn()
      const label = { explorer: 'Explorer', search: 'Search', history: 'History', settings: 'Settings' }[active]
      setup('/p/ns/proj/blob/doc.md', {
        rail: active,
        sidebarOpen: true,
        setRail,
        setSidebarOpen,
      })
      fireEvent.click(await screen.findByRole('button', { name: label }))
      expect(setSidebarOpen).toHaveBeenCalledWith(false)
      expect(setRail).not.toHaveBeenCalled()
    },
  )

  it('clicking an inactive item opens that pane (sets rail + opens)', async () => {
    const setRail = vi.fn()
    const setSidebarOpen = vi.fn()
    setup('/p/ns/proj/blob/doc.md', {
      rail: 'explorer',
      sidebarOpen: true,
      setRail,
      setSidebarOpen,
    })
    fireEvent.click(await screen.findByRole('button', { name: 'Search' }))
    expect(setRail).toHaveBeenCalledWith('search')
    expect(setSidebarOpen).toHaveBeenCalledWith(true)
  })

  it('clicking the active item while the pane is CLOSED reopens it', async () => {
    const setSidebarOpen = vi.fn()
    setup('/p/ns/proj/blob/doc.md', {
      rail: 'explorer',
      sidebarOpen: false,
      setSidebarOpen,
    })
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    expect(setSidebarOpen).toHaveBeenCalledWith(true)
  })
})

describe('useRailSelect — History opens the active file’s history (on open)', () => {
  it('from a file view, navigates to that file’s history route', async () => {
    const router = setup('/p/ns/proj/blob/reports/doc.md', { rail: 'explorer' })
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        '/p/ns/proj/history/reports/doc.md',
      ),
    )
  })

  it('with no file selected (project root), opens the history route empty', async () => {
    const router = setup('/p/ns/proj', { rail: 'explorer' })
    fireEvent.click(await screen.findByRole('button', { name: 'History' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toContain('/p/ns/proj/history'),
    )
  })
})

describe('useRailSelect — Explorer on a history route returns to /blob/<same file>', () => {
  it('from a sub-dir file history, navigates to that file’s blob view + rail Explorer', async () => {
    const setRail = vi.fn()
    const router = setup('/p/ns/proj/history/reports/doc.md', {
      rail: 'history',
      setRail,
    })
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/p/ns/proj/blob/reports/doc.md'),
    )
    expect(setRail).toHaveBeenCalledWith('explorer')
  })

  it('from a root file history, navigates to that file’s blob view', async () => {
    const router = setup('/p/ns/proj/history/doc.md', { rail: 'history' })
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/p/ns/proj/blob/doc.md'),
    )
  })

  it('on a history route with NO file, goes to the project root (not a broken route)', async () => {
    const router = setup('/p/ns/proj/history/', { rail: 'history' })
    fireEvent.click(await screen.findByRole('button', { name: 'Explorer' }))
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/p/ns/proj'),
    )
  })
})

describe('useResetRailToExplorerOnProjectChange', () => {
  function mountReset(url: string, setRail: (v: RailView) => void) {
    function Harness() {
      useResetRailToExplorerOnProjectChange(setRail)
      return null
    }
    const rootRoute = createRootRoute({ component: Harness })
    const mk = (path: string) =>
      createRoute({ getParentRoute: () => rootRoute, path, component: () => null })
    const router = createRouter({
      routeTree: rootRoute.addChildren([
        mk('/'),
        mk('/p/$namespace/$project'),
        mk('/p/$namespace/$project/blob/$'),
        mk('/p/$namespace/$project/history/$'),
      ]),
      history: createMemoryHistory({ initialEntries: [url] }),
    })
    render(<RouterProvider router={router as never} />)
    return router
  }

  it('defaults the rail to Explorer on entering a project', async () => {
    const setRail = vi.fn()
    mountReset('/p/ns/proj', setRail)
    await waitFor(() => expect(setRail).toHaveBeenCalledWith('explorer'))
  })

  it('does NOT reset when navigating among the same project’s files/history', async () => {
    const setRail = vi.fn()
    const router = mountReset('/p/ns/proj/blob/doc.md', setRail)
    await waitFor(() => expect(setRail).toHaveBeenCalledWith('explorer'))
    setRail.mockClear()
    await router.navigate({
      to: '/p/$namespace/$project/history/$',
      params: { namespace: 'ns', project: 'proj', _splat: 'doc.md' },
    })
    await new Promise((r) => setTimeout(r, 0))
    expect(setRail).not.toHaveBeenCalled()
  })

  it('resets again when switching to a different project', async () => {
    const setRail = vi.fn()
    const router = mountReset('/p/ns/projA', setRail)
    await waitFor(() => expect(setRail).toHaveBeenCalledWith('explorer'))
    setRail.mockClear()
    await router.navigate({
      to: '/p/$namespace/$project',
      params: { namespace: 'ns', project: 'projB' },
    })
    await waitFor(() => expect(setRail).toHaveBeenCalledWith('explorer'))
  })
})
