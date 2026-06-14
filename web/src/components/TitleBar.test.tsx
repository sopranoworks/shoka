import { render, screen, within } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
import {
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
} from '@tanstack/react-router'
import { PaletteProvider } from '../lib/palette'
import { TitleBar } from './TitleBar'

// Render the TitleBar inside a minimal memory-history router. The brand's
// <Link to="/"> and the breadcrumb crumb links resolve against the registered
// "/" and project routes; the PaletteProvider satisfies usePalette().
function renderTitleBar(initialPath: string) {
  const rootRoute = createRootRoute({
    component: () => (
      <PaletteProvider>
        <TitleBar onToggleSidebar={() => {}} />
      </PaletteProvider>
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
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute, projectRoute]),
    history: createMemoryHistory({ initialEntries: [initialPath] }),
  })
  // The test router is a distinct instance from the app's; cast to the
  // registered router type RouterProvider expects.
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

// The breadcrumb is a position trail continuing the brand "Shoka ›": it is
// derived purely from the route (pathname + ?ns=) so it can never disagree with
// the page. These assert the trail per route state (B-31).
describe('TitleBar breadcrumb', () => {
  it('renders no segment and NO brand chevron at the all-projects home "/"', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', { name: 'All projects' })
    // Brand reads as a bare "Shoka" — no dangling chevron when nothing follows.
    expect(brand.textContent).toBe('Shoka')
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' })
    expect(nav.textContent).toBe('')
    // The old wrong-term "repositories" crumb is gone for good.
    expect(screen.queryByText('repositories')).toBeNull()
  })

  it('reflects the ?ns= namespace filter as the current position', async () => {
    renderTitleBar('/?ns=shoka')
    const brand = await screen.findByRole('link', { name: 'All projects' })
    // A segment now follows the brand, so the chevron appears.
    expect(brand.textContent).toContain('›')
    const nav = screen.getByRole('navigation', { name: 'Breadcrumb' })
    const current = within(nav).getByText('shoka')
    expect(current).toHaveAttribute('aria-current', 'page')
    // The current position is a non-link span, not an anchor.
    expect(current.tagName).toBe('SPAN')
    expect(within(nav).queryByRole('link')).toBeNull()
    expect(screen.queryByText('repositories')).toBeNull()
  })

  it('builds namespace(link) / project(current) for a project route', async () => {
    renderTitleBar('/p/shoka/design')
    const nav = await screen.findByRole('navigation', { name: 'Breadcrumb' })
    // Ancestor namespace is a link back to its filtered list (/?ns=shoka).
    const nsLink = within(nav).getByRole('link', { name: 'shoka' })
    expect(nsLink.getAttribute('href')).toContain('ns=shoka')
    // The project is the current position: a non-link span, aria-current.
    const proj = within(nav).getByText('design')
    expect(proj).toHaveAttribute('aria-current', 'page')
    expect(within(nav).queryByRole('link', { name: 'design' })).toBeNull()
    expect(screen.queryByText('repositories')).toBeNull()
  })
})
