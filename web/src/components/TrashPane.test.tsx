import { render, screen, fireEvent } from '@testing-library/react'
import { describe, it, expect, vi } from 'vitest'
import { TrashPane } from './TrashPane'
import type { TrashItem } from '../lib/trashQueue'

function item(over: Partial<TrashItem> = {}): TrashItem {
  return {
    id: 't1',
    namespace: 'n',
    project: 'p',
    path: 'docs/a.md',
    etag: 'e1',
    deadline: Date.now() + 8_000,
    ...over,
  }
}

describe('TrashPane', () => {
  it('renders each queued file with its path, a live countdown, and a Cancel', () => {
    render(
      <TrashPane
        items={[item()]}
        onCancel={() => {}}
        onDeleteNow={() => {}}
        onClose={() => {}}
      />,
    )
    expect(screen.getByText('docs/a.md')).toBeInTheDocument()
    expect(screen.getByText(/Deleting in \d+s/)).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Cancel deleting docs/a.md' }),
    ).toBeInTheDocument()
  })

  // Directive #6 — structural mis-click safety: Cancel is the PROMINENT action;
  // the immediate "Delete now" is separated and non-prominent, so a mis-click
  // lands on "keep my file", never on destruction.
  it('#6 mis-click safety: Cancel is prominent; Delete-now is separated/non-prominent', () => {
    render(
      <TrashPane
        items={[item()]}
        onCancel={() => {}}
        onDeleteNow={() => {}}
        onClose={() => {}}
      />,
    )
    const cancel = screen.getByRole('button', { name: 'Cancel deleting docs/a.md' })
    const deleteNow = screen.getByRole('button', { name: 'Delete docs/a.md now' })
    expect(cancel).toHaveAttribute('data-prominent', 'true')
    expect(deleteNow).toHaveAttribute('data-prominent', 'false')
  })

  it('Cancel reverses the reservation (calls onCancel; never a delete)', () => {
    const onCancel = vi.fn()
    const onDeleteNow = vi.fn()
    render(
      <TrashPane
        items={[item({ id: 'x' })]}
        onCancel={onCancel}
        onDeleteNow={onDeleteNow}
        onClose={() => {}}
      />,
    )
    fireEvent.click(
      screen.getByRole('button', { name: 'Cancel deleting docs/a.md' }),
    )
    expect(onCancel).toHaveBeenCalledWith('x')
    expect(onDeleteNow).not.toHaveBeenCalled()
  })

  it('shows the empty state when nothing is queued', () => {
    render(
      <TrashPane
        items={[]}
        onCancel={() => {}}
        onDeleteNow={() => {}}
        onClose={() => {}}
      />,
    )
    expect(screen.getByText(/No files queued/)).toBeInTheDocument()
  })
})
