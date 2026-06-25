import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'

vi.mock('../../../packages/web-core/src/lib/queries', async (importOriginal) => {
  const orig = await importOriginal<Record<string, unknown>>()
  return {
    ...orig,
    useTreeQuery: () => ({
      data: [{ name: 'doc.md', path: 'doc.md', isFile: true }],
      isError: false,
    }),
  }
})

vi.mock('../../../packages/web-core/src/components/FileTree', () => ({
  FileTree: ({
    openMode,
    activePath,
  }: {
    openMode?: string
    activePath?: string | null
  }) => (
    <div
      data-testid="filetree"
      data-openmode={openMode}
      data-active={activePath ?? ''}
    />
  ),
  fileOpenRoute: (m: string) =>
    m === 'history'
      ? '/p/$namespace/$project/history/$'
      : '/p/$namespace/$project/blob/$',
  fileTreeStyles: {},
}))

import {
  Sidebar,
  ShellProvider,
  ContentProvider,
  useSimpleRailControls,
  useNoopRailReset,
  sidebarStyles,
} from '@shoka/web-core'

const minShell = {
  railItems: [],
  renderSidebar: () => null,
  useRailControls: useSimpleRailControls,
  useResetRailOnProjectChange: useNoopRailReset,
}

function renderSidebar(
  view: 'explorer' | 'history',
  url: string,
  withNewFile = false,
) {
  const contentConfig = withNewFile
    ? {
        renderNewFileButton: () => (
          <button type="button" className={sidebarStyles.newFileBtn} aria-label="New file">
            + New file
          </button>
        ),
      }
    : {}
  const rootRoute = createRootRoute({
    component: () => (
      <ShellProvider value={minShell}>
        <ContentProvider value={contentConfig}>
          <Sidebar view={view} />
        </ContentProvider>
      </ShellProvider>
    ),
  })
  const mk = (path: string) =>
    createRoute({ getParentRoute: () => rootRoute, path, component: () => null })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      mk('/'),
      mk('/p/$namespace/$project'),
      mk('/p/$namespace/$project/blob/$'),
      mk('/p/$namespace/$project/history/$'),
      mk('/p/$namespace/$project/new'),
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

describe('Sidebar History panel — tree retained, no cushion (B-31)', () => {
  it('renders the file tree (openMode="history") and NO "View history →" cushion', async () => {
    renderSidebar('history', '/p/ns/proj/history/doc.md')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'history')
    expect(tree).toHaveAttribute('data-active', 'doc.md')
    expect(screen.queryByText(/view history/i)).toBeNull()
  })

  it('with no file selected, still shows the tree (placeholder lives in the right pane)', async () => {
    renderSidebar('history', '/p/ns/proj')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'history')
    expect(tree).toHaveAttribute('data-active', '')
    expect(screen.queryByText(/view history/i)).toBeNull()
  })

  it('Explorer mode renders the same tree but opens files (openMode="blob")', async () => {
    renderSidebar('explorer', '/p/ns/proj/blob/doc.md')
    const tree = await screen.findByTestId('filetree')
    expect(tree).toHaveAttribute('data-openmode', 'blob')
    expect(tree).toHaveAttribute('data-active', 'doc.md')
  })
})

describe('Sidebar "+ New file" affordance — Explorer only (B-31 E)', () => {
  it('is present in Explorer mode when renderNewFileButton is provided', async () => {
    renderSidebar('explorer', '/p/ns/proj/blob/doc.md', true)
    const btn = await screen.findByRole('button', { name: 'New file' })
    expect(btn.className).toMatch(/newFileBtn/)
  })

  it('is NOT rendered in History mode (RED→GREEN)', async () => {
    renderSidebar('history', '/p/ns/proj/history/doc.md', true)
    await screen.findByTestId('filetree')
    expect(screen.queryByRole('button', { name: 'New file' })).toBeNull()
  })
})
