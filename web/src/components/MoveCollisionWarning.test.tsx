import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { MoveCollisionWarning } from './MoveCollisionWarning'

function setup() {
  const onCancel = vi.fn()
  const onOverwrite = vi.fn()
  const onSaveAs = vi.fn()
  render(
    <MoveCollisionWarning
      targetPath="docs/existing.md"
      onCancel={onCancel}
      onOverwrite={onOverwrite}
      onSaveAs={onSaveAs}
    />,
  )
  return { onCancel, onOverwrite, onSaveAs }
}

describe('MoveCollisionWarning', () => {
  it('offers exactly three actions and names the occupied target — and NO diff', () => {
    setup()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Save under a different name' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Overwrite' })).toBeInTheDocument()
    // A move collision is never a content diff.
    expect(screen.queryByRole('button', { name: /show diff/i })).toBeNull()
    expect(screen.getByText('docs/existing.md')).toBeInTheDocument()
  })

  it('Cancel and Save-as route immediately to their handlers', async () => {
    const user = userEvent.setup()
    const { onCancel, onSaveAs } = setup()
    await user.click(screen.getByRole('button', { name: 'Save under a different name' }))
    expect(onSaveAs).toHaveBeenCalledOnce()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onCancel).toHaveBeenCalledOnce()
  })

  it('Overwrite is gated by an inline confirm (no silent overwrite)', async () => {
    const user = userEvent.setup()
    const { onOverwrite } = setup()
    await user.click(screen.getByRole('button', { name: 'Overwrite' }))
    expect(onOverwrite).not.toHaveBeenCalled()
    expect(
      screen.getByRole('button', { name: 'Confirm overwrite' }),
    ).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Confirm overwrite' }))
    expect(onOverwrite).toHaveBeenCalledOnce()
  })
})
