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
import type { RailView } from './ActivityRail'

// Stub the tree query (non-empty so ProjectTree renders the tree) and FileTree
// (react-arborist needs ResizeObserver, unavailable in jsdom). The stub's host
// <div> identity is the probe: if FileTree is NOT remounted across a rail switch,
// React reconciles the SAME DOM node in place; if it IS remounted, a new node
// appears. (react-arborist holds its expansion state internally, seeded once at
// mount — so "not remounted" === "expansion preserved".)
vi.mock('../lib/queries', () => ({
  useTreeQuery: () => ({
    data: [{ name: 'doc.md', path: 'doc.md', isFile: true }],
    isError: false,
  }),
}))
vi.mock('./FileTree', () => ({
  FileTree: ({ openMode }: { openMode?: string }) => (
    <div data-testid="filetree" data-openmode={openMode} />
  ),
}))
// FileDropzone is an orthogonal native-file-drop wrapper (its own deps:
// QueryClient/Toast/DnD providers); these tests probe Sidebar's tree/rail logic,
// not the dropzone (covered by fileAdd.test.ts + the real-browser E2E). Stub it to
// a pass-through, mirroring the FileTree mock above.
vi.mock('./FileDropzone', () => ({
  FileDropzone: (props: { children?: unknown }) => <>{props.children as never}</>,
}))

import { Sidebar } from './Sidebar'

// Harness holds the rail `view` in state and exposes a button to flip Explorer↔
// History — mirroring Shell's rail toggle WITHOUT remounting Sidebar (Shell is
// persistent), so we isolate whether the rail switch remounts the tree.
function renderWithFlip(url: string) {
  function Harness() {
    const [view, setView] = useState<RailView>('explorer')
    return (
      <>
        <button onClick={() => setView('history')}>to-history</button>
        <button onClick={() => setView('explorer')}>to-explorer</button>
        <Sidebar view={view} />
      </>
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

// Defect 2 (RED→GREEN): the file tree must keep its expansion across Explorer↔
// History. The tree must NOT remount on the switch (a remount re-initialises
// react-arborist to all-collapsed — the 29→5 collapse). RED before: Sidebar
// returned different ExplorerView/HistoryView component types, so the tree
// remounted (a new DOM node) on each switch.
describe('Sidebar tree preserved across Explorer↔History (B-31)', () => {
  it('does NOT remount the FileTree when switching Explorer→History→Explorer', async () => {
    renderWithFlip('/p/ns/proj/blob/doc.md')
    const initial = await screen.findByTestId('filetree')
    expect(initial).toHaveAttribute('data-openmode', 'blob')

    fireEvent.click(screen.getByRole('button', { name: 'to-history' }))
    const afterHistory = screen.getByTestId('filetree')
    // Same DOM node ⇒ FileTree reconciled in place (not remounted) ⇒ arborist
    // expansion preserved. Only the openMode prop flipped.
    expect(afterHistory).toBe(initial)
    expect(afterHistory).toHaveAttribute('data-openmode', 'history')

    fireEvent.click(screen.getByRole('button', { name: 'to-explorer' }))
    const afterBack = screen.getByTestId('filetree')
    expect(afterBack).toBe(initial)
    expect(afterBack).toHaveAttribute('data-openmode', 'blob')
  })
})
