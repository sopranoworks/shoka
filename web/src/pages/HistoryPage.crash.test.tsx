import { render, screen, act } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

// Use the REAL useHistoryQuery (it calls useQuery → real React hooks) so a
// conditionally-called hook actually changes React's hook count. A plain mock of
// the query would contain no hooks and could not reproduce React #310. Stub only
// the socket so the queryFn never hits the network — it stays pending (Loading),
// which is all we need: the point is the component renders without a hooks error
// when a file is selected in History mode.
vi.mock('@shoka/web-core', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@shoka/web-core')>()),
  wsClient: () => ({ request: () => new Promise(() => {}) }),
}))

import { HistoryPage } from './HistoryPage'

function renderHistoryRouter(initial: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const rootRoute = createRootRoute()
  const historyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/history/$',
    validateSearch: (s: Record<string, unknown>) => s,
    component: HistoryPage,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([historyRoute]),
    history: createMemoryHistory({ initialEntries: [initial] }),
  })
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router as never} />
    </QueryClientProvider>,
  )
  return router
}

// Issue 1 (REGRESSION, RED→GREEN): selecting a file in History mode flips the
// HistoryPage path from '' to a value on the SAME instance. With the empty-path
// placeholder returning ABOVE useHistoryQuery, the hook count changed between the
// two renders → React error #310. The fix hoists the early return below all hooks.
describe('HistoryPage — no React #310 selecting a file in History mode', () => {
  it('does not crash going from no-file (placeholder) to a SUB-DIRECTORY file', async () => {
    const router = renderHistoryRouter('/p/shoka/maintenance/history/')
    expect(
      await screen.findByText(/select a file to see its history/i),
    ).toBeInTheDocument()
    // Re-render the same instance with a sub-dir path (the live repro). RED before
    // the fix: this throws #310 (more hooks than the previous render).
    await act(async () => {
      await router.navigate({
        to: '/p/$namespace/$project/history/$',
        params: {
          namespace: 'shoka',
          project: 'maintenance',
          _splat: 'reports/2026-06-15.md',
        },
      })
    })
    // No throw — the history view renders, showing the file path in its toolbar.
    expect(
      await screen.findByText('reports/2026-06-15.md'),
    ).toBeInTheDocument()
    expect(
      screen.queryByText(/select a file to see its history/i),
    ).toBeNull()
  })

  it('also handles a ROOT-LEVEL file (empty path → doc.md)', async () => {
    const router = renderHistoryRouter('/p/shoka/maintenance/history/')
    await act(async () => {
      await router.navigate({
        to: '/p/$namespace/$project/history/$',
        params: { namespace: 'shoka', project: 'maintenance', _splat: 'doc.md' },
      })
    })
    expect(await screen.findByText('doc.md')).toBeInTheDocument()
  })
})
