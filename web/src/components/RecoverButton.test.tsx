import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

const request = vi.fn()
vi.mock('../../../packages/web-core/src/lib/wsClient', async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  wsClient: () => ({ request }),
}))

import { RecoverButton, ToastProvider } from '@shoka/web-core'

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
  beforeEach(() => request.mockReset())

  it('re-syncs the named project and refreshes the projects badge', async () => {
    request.mockResolvedValueOnce({
      message: 'Re-synced to the on-disk HEAD; the project is healthy and writes are enabled.',
    })
    const { invalidate } = wrap(
      <RecoverButton namespace="shoka" project="maintenance" />,
    )

    fireEvent.click(
      screen.getByRole('button', { name: /recover shoka\/maintenance/i }),
    )

    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('RECOVER_PROJECT', {
        namespace: 'shoka',
        projectName: 'maintenance',
      }),
    )
    await waitFor(() =>
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ['projects'] }),
    )
  })

  it('still refreshes the badge when recovery reports genuine drift', async () => {
    request.mockResolvedValueOnce({
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
