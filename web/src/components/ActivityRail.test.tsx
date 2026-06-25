import { render, screen } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import {
  ActivityRail,
  ShellProvider,
  useNoopRailReset,
  useSimpleRailControls,
  type ShellConfig,
} from '@shoka/web-core'

const TEST_ITEMS: ShellConfig['railItems'] = [
  { id: 'explorer', label: 'Explorer', icon: <span>E</span> },
  { id: 'search', label: 'Search', icon: <span>S</span> },
  { id: 'history', label: 'History', icon: <span>H</span> },
  { id: 'settings', label: 'Settings', icon: <span>G</span> },
]

function testConfig(overrides: Partial<ShellConfig> = {}): ShellConfig {
  return {
    railItems: TEST_ITEMS,
    renderSidebar: () => null,
    useRailControls: useSimpleRailControls,
    useResetRailOnProjectChange: useNoopRailReset,
    ...overrides,
  }
}

function renderRail(
  props: Partial<Parameters<typeof ActivityRail>[0]> = {},
  config?: Partial<ShellConfig>,
) {
  return render(
    <ShellProvider value={testConfig(config)}>
      <ActivityRail
        active={props.active ?? 'explorer'}
        onSelect={props.onSelect ?? (() => {})}
        disabled={props.disabled}
      />
    </ShellProvider>,
  )
}

describe('ActivityRail', () => {
  it('renders Explorer, Search, History and Settings', () => {
    renderRail()
    expect(screen.getByRole('button', { name: 'Explorer' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Search' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'History' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Settings' })).toBeInTheDocument()
  })

  it('does NOT render a Namespaces item', () => {
    renderRail()
    expect(screen.queryByRole('button', { name: 'Namespaces' })).toBeNull()
  })

  it('renders exactly four activity-bar items (incl. the Settings gear)', () => {
    renderRail()
    const rail = screen.getByRole('navigation', { name: 'Activity bar' })
    expect(rail.querySelectorAll('button')).toHaveLength(4)
  })

  it('renders disabled items as inert (disabled + aria-disabled), others enabled', () => {
    const onSelect = vi.fn()
    renderRail({
      active: 'search',
      onSelect,
      disabled: ['search', 'history'],
    })
    const search = screen.getByRole('button', { name: 'Search' })
    const history = screen.getByRole('button', { name: 'History' })
    const explorer = screen.getByRole('button', { name: 'Explorer' })

    expect(search).toBeDisabled()
    expect(search).toHaveAttribute('aria-disabled', 'true')
    expect(search).toHaveAttribute('data-active', 'false')
    expect(history).toBeDisabled()
    expect(explorer).toBeEnabled()
    expect(explorer).toHaveAttribute('aria-disabled', 'false')
  })
})

describe('ActivityRail bottom section', () => {
  it('renders renderRailBottom content when provided', () => {
    renderRail({}, {
      renderRailBottom: () => <button aria-label="Custom">X</button>,
    })
    expect(screen.getByRole('button', { name: 'Custom' })).toBeInTheDocument()
  })

  it('renders no bottom section when renderRailBottom is omitted', () => {
    renderRail()
    const rail = screen.getByRole('navigation', { name: 'Activity bar' })
    const allButtons = rail.closest('[class]')!.querySelectorAll('button')
    expect(allButtons).toHaveLength(4)
  })
})
