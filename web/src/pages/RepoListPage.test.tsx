import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRouter,
  createRoute,
  createMemoryHistory,
} from '@tanstack/react-router'

vi.mock('../../../packages/web-core/src/lib/queries', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  useProjectsQuery: () => ({
    data: [],
    isPending: false,
    isError: false,
    error: null,
  }),
}))

import { RepoListPage } from '@shoka/web-core'

function renderPage() {
  const rootRoute = createRootRoute()
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: RepoListPage,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  return render(<RouterProvider router={router as never} />)
}

describe('RepoListPage terminology (B-31)', () => {
  it('uses "project" wording, not "repository"', async () => {
    const { container } = renderPage()
    expect(
      await screen.findByRole('heading', { name: 'Projects' }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Repositories' })).toBeNull()
    expect(container.textContent).not.toMatch(/repositor/i)
  })
})
