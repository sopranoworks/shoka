import { render, screen } from '@testing-library/react'
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
  it('is a link to "/" with an accessible "Back to repositories" name', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', {
      name: 'Back to repositories',
    })
    expect(brand).toHaveAttribute('href', '/')
  })

  it('renders the word "Shoka" and NOT the stray kanji 蕉 (U+8549)', async () => {
    renderTitleBar('/')
    const brand = await screen.findByRole('link', {
      name: 'Back to repositories',
    })
    expect(brand.textContent).toContain('Shoka')
    expect(brand.textContent).not.toContain('蕉')
    expect(brand.textContent).not.toContain(String.fromCodePoint(0x8549))
  })

  it('marks aria-current="page" on the list route', async () => {
    renderTitleBar('/')
    const onList = await screen.findByRole('link', {
      name: 'Back to repositories',
    })
    expect(onList).toHaveAttribute('aria-current', 'page')
  })

  it('does not mark the brand current when on a project route', async () => {
    renderTitleBar('/p/acme/widgets')
    const brand = await screen.findByRole('link', {
      name: 'Back to repositories',
    })
    expect(brand).not.toHaveAttribute('aria-current')
  })
})
