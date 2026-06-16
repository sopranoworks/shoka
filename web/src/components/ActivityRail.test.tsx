import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import { ActivityRail } from './ActivityRail'

// B-31: the activity bar carries only the useful items. The "Namespaces" item
// was removed (its icon read like VSCode's extensions icon and it opened the
// project list inside the file-explorer pane — an inconsistent surface). This
// test fails against the pre-fix rail (which had a fourth "Namespaces" button)
// and passes after, guarding against reintroduction.
describe('ActivityRail', () => {
  it('renders Explorer, Search, History and Settings', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    expect(screen.getByRole('button', { name: 'Explorer' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Search' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'History' })).toBeInTheDocument()
    // The Settings gear (B-28 stage 3) is always present.
    expect(screen.getByRole('button', { name: 'Settings' })).toBeInTheDocument()
  })

  it('does NOT render a Namespaces item', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    expect(screen.queryByRole('button', { name: 'Namespaces' })).toBeNull()
  })

  it('renders exactly four activity-bar items (incl. the Settings gear)', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    const rail = screen.getByRole('navigation', { name: 'Activity bar' })
    expect(rail.querySelectorAll('button')).toHaveLength(4)
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

// B-31 trash-can: a trash box at the bottom of the rail opens/collapses the trash
// pane and doubles as the drag-to-trash drop target. It is a SEPARATE surface
// from the activity items (it lives outside the nav region).
describe('ActivityRail trash box (B-31)', () => {
  it('renders a Trash box that is NOT one of the activity items', () => {
    render(<ActivityRail active="explorer" onSelect={() => {}} />)
    const nav = screen.getByRole('navigation', { name: 'Activity bar' })
    // The nav holds the four activity items (incl. Settings); trash lives outside it.
    expect(nav.querySelectorAll('button')).toHaveLength(4)
    expect(nav.querySelector('[aria-label="Trash"]')).toBeNull()
    expect(screen.getByRole('button', { name: 'Trash' })).toBeInTheDocument()
  })

  it('shows the queued-count badge only when items are pending', () => {
    const { rerender } = render(
      <ActivityRail active="explorer" onSelect={() => {}} trashCount={0} />,
    )
    expect(screen.queryByLabelText(/queued/)).toBeNull()
    rerender(
      <ActivityRail active="explorer" onSelect={() => {}} trashCount={3} />,
    )
    expect(screen.getByLabelText('3 queued')).toHaveTextContent('3')
  })

  it('opens/collapses the trash pane on click and reflects the open state', () => {
    const onTrashClick = vi.fn()
    render(
      <ActivityRail
        active="explorer"
        onSelect={() => {}}
        onTrashClick={onTrashClick}
        trashActive
      />,
    )
    const trash = screen.getByRole('button', { name: 'Trash' })
    expect(trash).toHaveAttribute('aria-pressed', 'true')
    fireEvent.click(trash)
    expect(onTrashClick).toHaveBeenCalledTimes(1)
  })

  // Drag-to-trash is now a react-dnd drop target (B-31 RE-OPEN): the rail attaches
  // react-dnd's drop connector via trashDropRef and reflects the drag-over state via
  // trashIsOver (the drop affordance). The end-to-end drop→enqueue is proven by the
  // real-browser E2E (tests/e2e/trash-dnd.spec.ts), not a jsdom synthetic event.
  it('attaches the react-dnd drop connector to the trash box', () => {
    const trashDropRef = vi.fn()
    render(
      <ActivityRail
        active="explorer"
        onSelect={() => {}}
        trashDropRef={trashDropRef}
      />,
    )
    // React calls the ref callback with the trash button node on mount.
    expect(trashDropRef).toHaveBeenCalled()
    const node = trashDropRef.mock.calls[0][0] as HTMLElement | null
    expect(node).not.toBeNull()
    expect(node).toHaveAttribute('aria-label', 'Trash')
  })

  it('reflects the drop affordance via data-drop-active', () => {
    const { rerender } = render(
      <ActivityRail active="explorer" onSelect={() => {}} trashIsOver={false} />,
    )
    expect(screen.getByRole('button', { name: 'Trash' })).toHaveAttribute(
      'data-drop-active',
      'false',
    )
    rerender(
      <ActivityRail active="explorer" onSelect={() => {}} trashIsOver={true} />,
    )
    expect(screen.getByRole('button', { name: 'Trash' })).toHaveAttribute(
      'data-drop-active',
      'true',
    )
  })
})
