import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import { ActivityRail } from './ActivityRail'

// B-31: the activity bar carries only the useful items. The "Namespaces" item
// was removed (its icon read like VSCode's extensions icon and it opened the
// project list inside the file-explorer pane — an inconsistent surface). This
// test fails against the pre-fix rail (which had a fourth "Namespaces" button)
// and passes after, guarding against reintroduction.
describe('ActivityRail', () => {
  it('renders Explorer, Search and History', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    expect(screen.getByRole('button', { name: 'Explorer' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Search' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'History' })).toBeInTheDocument()
  })

  it('does NOT render a Namespaces item', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    expect(screen.queryByRole('button', { name: 'Namespaces' })).toBeNull()
  })

  it('renders exactly three activity-bar items', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    const rail = screen.getByRole('navigation', { name: 'Activity bar' })
    expect(rail.querySelectorAll('button')).toHaveLength(3)
  })

  // Per-item disabled state (admin rail refinement): a disabled item is inert
  // (disabled + aria-disabled) and not active-highlighted, while the others stay
  // enabled.
  it('renders disabled items as inert (disabled + aria-disabled), others enabled', () => {
    const onSelect = vi.fn()
    render(
      <ActivityRail
        active="search"
        onSelect={onSelect}
        disabled={['search', 'history']}
      />,
    )
    const search = screen.getByRole('button', { name: 'Search' })
    const history = screen.getByRole('button', { name: 'History' })
    const explorer = screen.getByRole('button', { name: 'Explorer' })

    expect(search).toBeDisabled()
    expect(search).toHaveAttribute('aria-disabled', 'true')
    // Disabled wins over active: even as the active pane, it is not highlighted.
    expect(search).toHaveAttribute('data-active', 'false')
    expect(history).toBeDisabled()
    expect(explorer).toBeEnabled()
    expect(explorer).toHaveAttribute('aria-disabled', 'false')
  })
})
