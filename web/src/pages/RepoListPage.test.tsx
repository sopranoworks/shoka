import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'

// RepoListPage reads its namespace filter from the index route and its data from
// the projects query. Both are mocked so the page can render standalone without
// the full app router/Shell — we are asserting its user-facing wording, not its
// data path. With an empty (settled) project list the page renders only its
// header chrome and the "no projects" state, so no <Link> cards are built and no
// live router context is needed.
vi.mock('../router', () => ({
  indexRoute: {
    useSearch: () => ({}),
    useNavigate: () => () => {},
  },
}))
vi.mock('../lib/queries', () => ({
  useProjectsQuery: () => ({
    data: [],
    isPending: false,
    isError: false,
    error: null,
  }),
}))

import { RepoListPage } from './RepoListPage'

describe('RepoListPage terminology (B-31)', () => {
  it('uses Shoka’s "project" wording, not "repository"', () => {
    const { container } = render(<RepoListPage />)
    // Heading is "Projects", not the wrong-term "Repositories".
    expect(
      screen.getByRole('heading', { name: 'Projects' }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Repositories' })).toBeNull()
    // No "repository"/"Repositories" wording anywhere in the rendered chrome.
    expect(container.textContent).not.toMatch(/repositor/i)
  })
})
