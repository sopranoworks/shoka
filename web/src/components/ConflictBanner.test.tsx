import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { ConflictBanner } from './ConflictBanner'

function setup(extra?: { onSaveAs?: () => void; onShowDiff?: () => void }) {
  const onDiscardYours = vi.fn()
  const onForceOverwrite = vi.fn()
  render(
    <ConflictBanner
      message="File was modified by someone else"
      onDiscardYours={onDiscardYours}
      onForceOverwrite={onForceOverwrite}
      onSaveAs={extra?.onSaveAs}
      onShowDiff={extra?.onShowDiff}
    />,
  )
  return { onDiscardYours, onForceOverwrite }
}

describe('ConflictBanner', () => {
  it('always offers Discard and Force; Save-as/Show-diff only when handlers are given', () => {
    setup()
    expect(screen.getByRole('button', { name: 'Discard mine, load latest' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Force overwrite' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save as…' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Show diff' })).toBeNull()
  })

  it('renders Save-as and Show-diff and routes them to their handlers', async () => {
    const user = userEvent.setup()
    const onSaveAs = vi.fn()
    const onShowDiff = vi.fn()
    setup({ onSaveAs, onShowDiff })
    await user.click(screen.getByRole('button', { name: 'Save as…' }))
    await user.click(screen.getByRole('button', { name: 'Show diff' }))
    expect(onSaveAs).toHaveBeenCalledOnce()
    expect(onShowDiff).toHaveBeenCalledOnce()
  })

  it('Discard routes immediately to its handler', async () => {
    const user = userEvent.setup()
    const { onDiscardYours } = setup()
    await user.click(screen.getByRole('button', { name: 'Discard mine, load latest' }))
    expect(onDiscardYours).toHaveBeenCalledOnce()
  })

  it('Force overwrite is gated by an inline confirm', async () => {
    const user = userEvent.setup()
    const { onForceOverwrite } = setup()

    await user.click(screen.getByRole('button', { name: 'Force overwrite' }))
    // Not yet — a confirm step appears first.
    expect(onForceOverwrite).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Confirm overwrite' })).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Confirm overwrite' }))
    expect(onForceOverwrite).toHaveBeenCalledOnce()
  })

  it('the inline force-confirm can be cancelled without overwriting', async () => {
    const user = userEvent.setup()
    const { onForceOverwrite } = setup()
    await user.click(screen.getByRole('button', { name: 'Force overwrite' }))
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onForceOverwrite).not.toHaveBeenCalled()
    // Back to the single Force overwrite affordance.
    expect(screen.getByRole('button', { name: 'Force overwrite' })).toBeInTheDocument()
  })
})
