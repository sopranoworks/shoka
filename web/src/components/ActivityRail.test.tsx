import { render, screen } from '@testing-library/react'
import { describe, it, expect } from 'vitest'
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
})
