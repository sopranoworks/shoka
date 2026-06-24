import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { ToastProvider } from '@shoka/web-core'

const recoverProject = vi.fn()
vi.mock('../lib/fileOps', () => ({
  recoverProject: (ns: string, p: string) => recoverProject(ns, p),
}))

import { RecoverButton } from './RecoverButton'

function wrap(ui: ReactNode) {
  const qc = new QueryClient()
  const invalidate = vi.spyOn(qc, 'invalidateQueries')
  return {
    invalidate,
    ...render(
      <QueryClientProvider client={qc}>
        <ToastProvider>{ui}</ToastProvider>
      </QueryClientProvider>,
    ),
  }
}

describe('RecoverButton', () => {
  beforeEach(() => recoverProject.mockReset())

  it('re-syncs the named project and refreshes the projects badge', async () => {
    recoverProject.mockResolvedValueOnce({
      namespace: 'shoka',
      project: 'maintenance',
      state: 'healthy',
      recovered: true,
      message: 'Re-synced to the on-disk HEAD; the project is healthy and writes are enabled.',
    })
    const { invalidate } = wrap(
      <RecoverButton namespace="shoka" project="maintenance" />,
    )

    fireEvent.click(
      screen.getByRole('button', { name: /recover shoka\/maintenance/i }),
    )

    await waitFor(() =>
      expect(recoverProject).toHaveBeenCalledWith('shoka', 'maintenance'),
    )
    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ['projects'] }),
    )
  })

  it('still refreshes the badge when recovery reports genuine drift', async () => {
    recoverProject.mockResolvedValueOnce({
      namespace: 'shoka',
      project: 'maintenance',
      state: 'corrupted',
      recovered: false,
      message: 'genuine uncommitted drift; use accept-working-tree or accept-head',
    })
    const { invalidate } = wrap(
      <RecoverButton namespace="shoka" project="maintenance" />,
    )
    fireEvent.click(screen.getByRole('button', { name: /recover/i }))
    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ['projects'] }),
    )
  })
})
