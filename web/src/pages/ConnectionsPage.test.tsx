import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { ToastProvider } from '../lib/toast'
import { AdminProvider } from '../lib/admin'

// Mock the imperative ops; keep the real clientDomain + OAuthDeniedError so the
// page's URL parsing and typed-error handling are exercised, not stubbed. The
// fns are defined via vi.hoisted so the hoisted vi.mock factory can reference
// them without hitting the temporal-dead-zone (the factory builds its object
// eagerly during import).
const { listConnections, revokeConnection } = vi.hoisted(() => ({
  listConnections: vi.fn(),
  revokeConnection: vi.fn(),
}))
vi.mock('../lib/oauthOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/oauthOps')>()
  return { ...actual, listConnections, revokeConnection }
})

import { ConnectionsPage } from './ConnectionsPage'
import { OAuthDeniedError } from '../lib/oauthOps'

const sampleConn = {
  series_id: 'series-aaaa-1111',
  series_id_short: 'series-a',
  client_id: 'https://connector.example.com/cimd',
  principal_name: 'Op Erator',
  principal_email: 'op@example.test',
  issued_at: '2026-06-03T12:00:00Z',
  access_expiry: '2026-06-03T13:00:00Z',
}

function renderPage(admin = true): { container: HTMLElement } {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <AdminProvider value={admin}>{children}</AdminProvider>
      </ToastProvider>
    </QueryClientProvider>
  )
  return render(<ConnectionsPage />, { wrapper })
}

describe('ConnectionsPage', () => {
  beforeEach(() => {
    listConnections.mockReset()
    revokeConnection.mockReset()
  })

  it('lists connections by client domain, principal, and short series id (no secrets)', async () => {
    listConnections.mockResolvedValue([sampleConn])
    const { container } = renderPage()

    // Client identity is shown as its CIMD domain (not the full URL, not a token).
    expect(await screen.findByText('connector.example.com')).toBeInTheDocument()
    expect(screen.getByText('Op Erator')).toBeInTheDocument()
    expect(screen.getByText('series-a')).toBeInTheDocument()
    // No secret value anywhere in the rendered DOM: no token text, and the full
    // (long) series id is never shown — only its short prefix.
    expect(container.textContent).not.toMatch(/token/i)
    expect(container.textContent).not.toContain('series-aaaa-1111')
  })

  it('shows the empty state when there are no connections', async () => {
    listConnections.mockResolvedValue([])
    renderPage()
    expect(
      await screen.findByText('No active OAuth connections.'),
    ).toBeInTheDocument()
  })

  it('hides the surface and does not query for a non-admin', async () => {
    listConnections.mockResolvedValue([sampleConn])
    renderPage(false)
    expect(
      await screen.findByText(/not authorized to manage/i),
    ).toBeInTheDocument()
    expect(listConnections).not.toHaveBeenCalled()
  })

  it('reports the oauth-disabled refusal as a clear state, not a generic error', async () => {
    listConnections.mockRejectedValue(
      new OAuthDeniedError('oauth_disabled', 'off'),
    )
    renderPage()
    expect(
      await screen.findByText(/OAuth is not enabled on this server/i),
    ).toBeInTheDocument()
  })

  it('revoke is inline-confirm gated, targets one series, and drops the row on success', async () => {
    const user = userEvent.setup()
    // First load: one connection; after revoke the refetch returns none.
    listConnections
      .mockResolvedValueOnce([sampleConn])
      .mockResolvedValue([])
    revokeConnection.mockResolvedValue(undefined)

    renderPage()
    await screen.findByText('connector.example.com')

    // First click only arms the confirm (no revoke yet) — destructive, gated.
    await user.click(screen.getByRole('button', { name: 'Revoke' }))
    expect(revokeConnection).not.toHaveBeenCalled()
    const confirm = screen.getByRole('button', { name: 'Confirm revoke' })

    await user.click(confirm)
    expect(revokeConnection).toHaveBeenCalledWith('series-aaaa-1111')

    // On success the list invalidates and the row drops.
    await waitFor(() =>
      expect(screen.queryByText('connector.example.com')).toBeNull(),
    )
    expect(
      screen.getByText('No active OAuth connections.'),
    ).toBeInTheDocument()
  })
})
