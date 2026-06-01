import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi } from 'vitest'
import { ExternalChangeBanner } from './ExternalChangeBanner'

function setup(kind: 'write' | 'delete') {
  const h = {
    onResolve: vi.fn(),
    onSaveAsNew: vi.fn(),
    onDiscardToTree: vi.fn(),
    onDismiss: vi.fn(),
  }
  render(<ExternalChangeBanner kind={kind} {...h} />)
  return h
}

describe('ExternalChangeBanner', () => {
  it('write: offers Resolve now + Dismiss (no delete-only actions)', async () => {
    const user = userEvent.setup()
    const h = setup('write')
    expect(screen.getByText('This file was modified by someone else.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save mine as new file' })).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Resolve now' }))
    expect(h.onResolve).toHaveBeenCalledOnce()
    await user.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(h.onDismiss).toHaveBeenCalledOnce()
  })

  it('delete: offers Save-mine-as-new, Discard-to-tree, and Dismiss', async () => {
    const user = userEvent.setup()
    const h = setup('delete')
    expect(screen.getByText('This file was deleted by someone else.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Resolve now' })).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Save mine as new file' }))
    await user.click(screen.getByRole('button', { name: 'Discard mine, go to tree' }))
    expect(h.onSaveAsNew).toHaveBeenCalledOnce()
    expect(h.onDiscardToTree).toHaveBeenCalledOnce()
  })
})
