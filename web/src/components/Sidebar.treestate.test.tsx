import { useState } from 'react'
import { render, screen, fireEvent } from '@testing-library/react'
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
  FileTree: ({ openMode }: { openMode?: string }) => (
    <div data-testid="filetree" data-openmode={openMode} />
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
} from '@shoka/web-core'

const minShell = {
  railItems: [],
  renderSidebar: () => null,
  useRailControls: useSimpleRailControls,
  useResetRailOnProjectChange: useNoopRailReset,
}

function renderWithFlip(url: string) {
  function Harness() {
    const [view, setView] = useState<string>('explorer')
    return (
      <ShellProvider value={minShell}>
        <ContentProvider>
          <button onClick={() => setView('history')}>to-history</button>
          <button onClick={() => setView('explorer')}>to-explorer</button>
          <Sidebar view={view} />
        </ContentProvider>
      </ShellProvider>
    )
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
      mk('/p/$namespace/$project/new'),
    ]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
  return router
}

describe('Sidebar tree preserved across Explorer↔History (B-31)', () => {
  it('does NOT remount the FileTree when switching Explorer→History→Explorer', async () => {
    renderWithFlip('/p/ns/proj/blob/doc.md')
    const initial = await screen.findByTestId('filetree')
    expect(initial).toHaveAttribute('data-openmode', 'blob')

    fireEvent.click(screen.getByRole('button', { name: 'to-history' }))
    const afterHistory = screen.getByTestId('filetree')
    expect(afterHistory).toBe(initial)
    expect(afterHistory).toHaveAttribute('data-openmode', 'history')

    fireEvent.click(screen.getByRole('button', { name: 'to-explorer' }))
    const afterBack = screen.getByTestId('filetree')
    expect(afterBack).toBe(initial)
    expect(afterBack).toHaveAttribute('data-openmode', 'blob')
  })
})
