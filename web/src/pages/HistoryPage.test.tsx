import { render, screen, within } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'

// The History view reads all three of its data sets over the /ws/ui queries;
// mock them so we assert the rendered view (commit list / version / diff /
// suppressed banner), not the data path.
const useHistoryQuery = vi.fn()
const useFileAtQuery = vi.fn()
const useDiffQuery = vi.fn()
vi.mock('../../../packages/web-core/src/lib/queries', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  useHistoryQuery: (...a: unknown[]) => useHistoryQuery(...a),
  useFileAtQuery: (...a: unknown[]) => useFileAtQuery(...a),
  useDiffQuery: (...a: unknown[]) => useDiffQuery(...a),
}))

import { HistoryPage } from '@shoka/web-core'

const COMMITS = [
  {
    hash: '1111111111111111111111111111111111111111',
    subject: 'Update doc.md',
    committer: 'Shoka Operator',
    commitDate: '2026-06-14T12:00:00Z',
  },
  {
    hash: '2222222222222222222222222222222222222222',
    subject: 'Create doc.md',
    committer: 'Aki Tanaka',
    commitDate: '2026-06-13T09:30:00Z',
  },
]

function renderHistory(url: string) {
  const rootRoute = createRootRoute()
  const historyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/history/$',
    validateSearch: (s: Record<string, unknown>) => s,
    component: HistoryPage,
  })
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([historyRoute, indexRoute]),
    history: createMemoryHistory({ initialEntries: [url] }),
  })
  render(<RouterProvider router={router as never} />)
}

beforeEach(() => {
  useHistoryQuery.mockReturnValue({ data: { commits: COMMITS }, isError: false })
  useFileAtQuery.mockReturnValue({
    data: { path: 'doc.md', hash: COMMITS[0].hash, content: '# Title\n\nbody\n' },
    isError: false,
  })
  useDiffQuery.mockReturnValue({
    data: {
      path: 'doc.md',
      fromHash: COMMITS[1].hash,
      toHash: COMMITS[0].hash,
      status: 'modified',
      binary: false,
      suppressed: '',
      hunks: [
        {
          oldStart: 1,
          oldLines: 2,
          newStart: 1,
          newLines: 2,
          lines: [
            { op: 'equal', text: 'line1' },
            { op: 'delete', text: 'old-line2' },
            { op: 'add', text: 'new-line2' },
          ],
        },
      ],
    },
    isError: false,
  })
})

describe('HistoryPage (B-31 phase 2)', () => {
  it('renders the commit list with subject + committer + date, and no changed-file list', async () => {
    renderHistory('/p/ns/proj/history/doc.md')

    const list = await screen.findByRole('complementary', {
      name: 'Commit history',
    })
    // Both commits' subjects and committers render.
    expect(within(list).getByText('Update doc.md')).toBeInTheDocument()
    expect(within(list).getByText('Create doc.md')).toBeInTheDocument()
    expect(within(list).getByText('Shoka Operator')).toBeInTheDocument()
    expect(within(list).getByText('Aki Tanaka')).toBeInTheDocument()
    // Short hash (8 chars) shown.
    expect(within(list).getByText('11111111')).toBeInTheDocument()
    // Single-file commits ⇒ no changed-file list / shortstat anywhere.
    expect(list.textContent).not.toMatch(/files? changed/i)
    expect(list.textContent).not.toMatch(/\d+\s*\+\+/)
  })

  it('renders a version’s content in Version mode', async () => {
    renderHistory('/p/ns/proj/history/doc.md?mode=version')
    // The mocked v-newest content renders (markdown heading -> <h1>Title</h1>).
    expect(await screen.findByRole('heading', { name: 'Title' })).toBeInTheDocument()
    expect(screen.getByText('body')).toBeInTheDocument()
    expect(useFileAtQuery).toHaveBeenCalled()
  })

  it('renders a structured diff with add/delete lines in Diff mode', async () => {
    renderHistory('/p/ns/proj/history/doc.md?mode=diff')
    // The hunk header and the changed lines render.
    expect(await screen.findByText(/@@ -1,2 \+1,2 @@/)).toBeInTheDocument()
    expect(screen.getByText('old-line2')).toBeInTheDocument()
    expect(screen.getByText('new-line2')).toBeInTheDocument()
  })

  it('shows a suppression banner instead of an empty diff when Suppressed is set', async () => {
    useDiffQuery.mockReturnValue({
      data: {
        path: 'doc.md',
        fromHash: COMMITS[1].hash,
        toHash: COMMITS[0].hash,
        status: 'modified',
        binary: true,
        suppressed: 'binary',
        hunks: [],
      },
      isError: false,
    })
    renderHistory('/p/ns/proj/history/doc.md?mode=diff')
    expect(await screen.findByRole('status')).toHaveTextContent(/binary/i)
  })

  // Fix A: History opened with no file selected (empty path) shows a quiet
  // placeholder in the right pane rather than erroring (the tree stays visible to
  // pick a file from).
  it('shows a quiet placeholder when no file is selected (empty path)', async () => {
    renderHistory('/p/ns/proj/history/')
    expect(
      await screen.findByText(/select a file to see its history/i),
    ).toBeInTheDocument()
    // No commit-history list is rendered in the placeholder state.
    expect(
      screen.queryByRole('complementary', { name: 'Commit history' }),
    ).toBeNull()
  })
})
