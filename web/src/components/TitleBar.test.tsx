import { render, screen, within } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { PaletteProvider, ShellProvider, TitleBar } from '@shoka/web-core'
import { shokaShellConfig } from '../shokaShellConfig'

function renderTitleBar(initialPath: string) {
  const rootRoute = createRootRoute({
    component: () => (
      <ShellProvider value={shokaShellConfig}>
        <PaletteProvider>
          <TitleBar onToggleSidebar={() => {}} />
        </PaletteProvider>
      </ShellProvider>
    ),
  })
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
  const historyRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/history/$',
    component: () => null,
  })
  const blobRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/blob/$',
    component: () => null,
  })
  const searchRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/p/$namespace/$project/search',
    component: () => null,
  })
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      indexRoute,
      projectRoute,
      historyRoute,
      blobRoute,
      searchRoute,
    ]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  })
  render(<RouterProvider router={router as never} />)
}

describe('TitleBar brand', () => {
  it('is a link to "/" with an accessible "All projects" name', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', {
      name: 'All projects',
    })
    expect(brand).toHaveAttribute('href', '/')
  })

  it('renders the word "Shoka" and NOT the stray kanji 蕉 (U+8549)', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', {
      name: 'All projects',
    })
    expect(brand.textContent).toContain('Shoka')
    expect(brand.textContent).not.toContain('蕉')
    expect(brand.textContent).not.toContain(String.fromCodePoint(0x8549))
  })

  it('marks aria-current="page" on the list route', async () => {
    renderTitleBar('/')
    const onList = await screen.findByRole('link', {
      name: 'All projects',
    })
    expect(onList).toHaveAttribute('aria-current', 'page')
  })

  it('does not mark the brand current when on a project route', async () => {
    renderTitleBar('/p/acme/widgets')
    const brand = await screen.findByRole('link', {
      name: 'All projects',
    })
    expect(brand).not.toHaveAttribute('aria-current')
  })
})

describe('TitleBar breadcrumb', () => {
  it('renders no segment and NO brand chevron at the all-projects home "/"', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', { name: 'All projects' })
    expect(brand.textContent).toBe('Shoka')
    expect(screen.queryByRole('navigation', { name: 'Breadcrumb' })).toBeNull()
    expect(screen.queryByText('repositories')).toBeNull()
  })

  it('reflects the ?ns= namespace filter as the current position', async () => {
    renderTitleBar('/?ns=shoka')
    const brand = await screen.findByRole('link', { name: 'All projects' })
    expect(brand.textContent).toBe('Shoka')
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    const current = within(nav).getByText('shoka')
    expect(current).toHaveAttribute('aria-current', 'page')
    expect(current.tagName).toBe('SPAN')
    expect(within(nav).queryByRole('link')).toBeNull()
    expect(screen.queryByText('repositories')).toBeNull()
  })

  it('builds namespace(link) / project(current) for a project route', async () => {
    renderTitleBar('/p/shoka/design')
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    const nsLink = within(nav).getByRole('link', { name: 'shoka' })
    expect(nsLink.getAttribute('href')).toContain('ns=shoka')
    const proj = within(nav).getByText('design')
    expect(proj).toHaveAttribute('aria-current', 'page')
    expect(within(nav).queryByRole('link', { name: 'design' })).toBeNull()
    expect(screen.queryByText('repositories')).toBeNull()
  })

  it('keeps the full trail on a history route', async () => {
    renderTitleBar('/p/shoka/design/history/spec.md')
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    expect(within(nav).getByRole('link', { name: 'shoka' })).toBeInTheDocument()
    expect(within(nav).getByRole('link', { name: 'design' })).toBeInTheDocument()
    const file = within(nav).getByText('spec.md')
    expect(file).toHaveAttribute('aria-current', 'page')
  })
})

describe('TitleBar breadcrumb — search route', () => {
  it('keeps namespace/project crumbs on the search route (not collapsed to just Shoka)', async () => {
    renderTitleBar('/p/shoka/design/search')
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    expect(within(nav).getByRole('link', { name: 'shoka' })).toBeInTheDocument()
    const proj = within(nav).getByText('design')
    expect(proj).toHaveAttribute('aria-current', 'page')
  })
})

describe('TitleBar breadcrumb — sub-directory segment (B-31 fix)', () => {
  const url = '/p/shoka/maintenance/blob/reports/2026-06-15.md'

  it('renders the sub-dir crumb as plain text, NOT a broken blob/<dir> link (RED→GREEN)', async () => {
    renderTitleBar(url)
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    expect(within(nav).queryByRole('link', { name: 'reports' })).toBeNull()
    expect(within(nav).getByText('reports')).toBeInTheDocument()
    const links = within(nav).getAllByRole('link')
    for (const a of links) {
      expect(a.getAttribute('href') ?? '').not.toContain('/blob/reports')
    }
  })

  it('marks exactly one aria-current — the final file segment, not ancestors', async () => {
    renderTitleBar(url)
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    const current = nav.querySelectorAll('[aria-current="page"]')
    expect(current).toHaveLength(1)
    expect(current[0].textContent).toBe('2026-06-15.md')
    const proj = within(nav).getByRole('link', { name: 'maintenance' })
    expect(proj).not.toHaveAttribute('aria-current')
  })

  it('keeps the root/project/ns crumbs working (no 01e8a0f regression)', async () => {
    renderTitleBar(url)
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    expect(
      within(nav).getByRole('link', { name: 'shoka' }).getAttribute('href'),
    ).toContain('ns=shoka')
    expect(
      within(nav).getByRole('link', { name: 'maintenance' }).getAttribute('href'),
    ).toContain('/p/shoka/maintenance')
  })
})
