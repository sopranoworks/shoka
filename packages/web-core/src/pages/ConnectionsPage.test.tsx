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
const { listConnections, revokeConnection, issueSelfToken } = vi.hoisted(() => ({
  listConnections: vi.fn(),
  revokeConnection: vi.fn(),
  issueSelfToken: vi.fn(),
}))
vi.mock('../lib/oauthOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/oauthOps')>()
  return { ...actual, listConnections, revokeConnection, issueSelfToken }
})

// B-71 Stage 2d: the page now also fetches the dynamic "domain" entries (the
// Trusted-domains section). Mock the domain ops; tests default to "no domains" so
// connections render in the self-issued section unless a test seeds a domain.
const { listDomains } = vi.hoisted(() => ({ listDomains: vi.fn() }))
vi.mock('../lib/domainOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/domainOps')>()
  return { ...actual, listDomains }
})

// B-71 Stage 3: the page also fetches confidential clients (the Confidential-clients section).
const { listConfidentialClients } = vi.hoisted(() => ({ listConfidentialClients: vi.fn() }))
vi.mock('../lib/confidentialOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/confidentialOps')>()
  return { ...actual, listConfidentialClients }
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
  scope: '*',
  domain: '', // self-issued / unattributed → the self section
}

// isoFromNow builds an access_expiry relative to the current instant so the
// client-derived status (expired/near/healthy) is deterministic regardless of
// when the suite runs.
function isoFromNow(ms: number): string {
  return new Date(Date.now() + ms).toISOString()
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
    issueSelfToken.mockReset()
    listDomains.mockReset()
    listDomains.mockResolvedValue([]) // default: no trusted domains
    listConfidentialClients.mockReset()
    listConfidentialClients.mockResolvedValue([]) // default: no confidential clients
  })

  it('lists connections by client domain, principal, and short series id (no secrets)', async () => {
    listConnections.mockResolvedValue([sampleConn])
    const { container } = renderPage()

    // Client identity is shown as its CIMD domain (not the full URL, not a token).
    expect(await screen.findByText('connector.example.com')).toBeInTheDocument()
    expect(screen.getByText('Op Erator')).toBeInTheDocument()
    expect(screen.getByText('series-a')).toBeInTheDocument()
    // No secret value anywhere in the rendered DOM: no secret-bearing field, and
    // the full (long) series id is never shown — only its short prefix. (The
    // "Generate CLI token" affordance is chrome, not a secret, so the guard
    // targets actual secret fields/values rather than the bare word "token".)
    expect(container.textContent).not.toMatch(/access_token|refresh_token/i)
    expect(container.textContent).not.toContain('series-aaaa-1111')
  })

  it('shows the empty states when there are no domains and no connections', async () => {
    listConnections.mockResolvedValue([])
    renderPage()
    // The grouped view (B-71 Stage 2d) composes two sections: trusted domains and
    // self-issued/other — each with its own empty state.
    expect(
      await screen.findByText(/No trusted domains configured/i),
    ).toBeInTheDocument()
    expect(screen.getByText('No self-issued tokens.')).toBeInTheDocument()
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

  it('generates a CLI token and shows it once with a copy control', async () => {
    const user = userEvent.setup()
    listConnections.mockResolvedValue([])
    issueSelfToken.mockResolvedValue({
      access_token: 'minted-secret-value',
      access_expiry: '2026-06-06T13:00:00Z',
    })

    renderPage()
    await screen.findByText('No self-issued tokens.')

    // The token is not shown until generated.
    expect(screen.queryByText('minted-secret-value')).toBeNull()

    await user.click(
      screen.getByRole('button', { name: 'Generate a token for the CLI' }),
    )

    // The minted token appears once, with copy + done controls.
    expect(await screen.findByText('minted-secret-value')).toBeInTheDocument()
    expect(issueSelfToken).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', { name: 'Copy token' })).toBeInTheDocument()

    // Dismissing clears it from the page.
    await user.click(screen.getByRole('button', { name: 'Dismiss token' }))
    expect(screen.queryByText('minted-secret-value')).toBeNull()
  })

  it('reports a denied token mint without showing a token', async () => {
    const user = userEvent.setup()
    listConnections.mockResolvedValue([])
    issueSelfToken.mockRejectedValue(
      new OAuthDeniedError('forbidden', 'admin only'),
    )

    renderPage()
    await screen.findByText('No self-issued tokens.')
    await user.click(
      screen.getByRole('button', { name: 'Generate a token for the CLI' }),
    )

    // The denial surfaces (as a toast); no token panel appears.
    await waitFor(() => expect(issueSelfToken).toHaveBeenCalled())
    expect(screen.queryByRole('button', { name: 'Copy token' })).toBeNull()
  })

  it('marks expired connections red and near-expiry orange, leaving healthy quiet', async () => {
    listConnections.mockResolvedValue([
      { ...sampleConn, series_id: 's-exp', client_id: 'https://exp.example/c', access_expiry: isoFromNow(-60 * 60 * 1000) },
      { ...sampleConn, series_id: 's-near', client_id: 'https://near.example/c', access_expiry: isoFromNow(8 * 60 * 1000) },
      { ...sampleConn, series_id: 's-ok', client_id: 'https://ok.example/c', access_expiry: isoFromNow(5 * 60 * 60 * 1000) },
    ])
    const { container } = renderPage()
    await screen.findByText('exp.example')

    // Exactly one cell of each status — the derivation maps each row correctly.
    expect(container.querySelectorAll('[data-status="expired"]')).toHaveLength(1)
    expect(container.querySelectorAll('[data-status="near"]')).toHaveLength(1)
    expect(container.querySelectorAll('[data-status="healthy"]')).toHaveLength(1)

    // The attention treatment shows a relative-time badge for the problem rows;
    // the healthy row stays quiet (no badge).
    expect(screen.getByText(/expired .* ago/)).toBeInTheDocument()
    expect(screen.getByText(/expires in/)).toBeInTheDocument()
    const healthyCell = container.querySelector('[data-status="healthy"]')
    expect(healthyCell?.querySelector('span:nth-child(2)')).toBeNull()
  })

  it('shows the token Scope column ("*" as all access; a scoped value verbatim)', async () => {
    listConnections.mockResolvedValue([
      { ...sampleConn, series_id: 's-star', client_id: 'https://star.example/c', scope: '*' },
      { ...sampleConn, series_id: 's-scoped', client_id: 'https://scoped.example/c', scope: 'namespace:foo' },
    ])
    renderPage()
    expect(await screen.findByText('all access')).toBeInTheDocument()
    expect(screen.getByText('namespace:foo')).toBeInTheDocument()
  })

  it('renders connections in the order the endpoint returns (server sorts newest-first)', async () => {
    listConnections.mockResolvedValue([
      { ...sampleConn, series_id: 's-new', client_id: 'https://newer.example/c' },
      { ...sampleConn, series_id: 's-old', client_id: 'https://older.example/c' },
    ])
    const { container } = renderPage()
    await screen.findByText('newer.example')
    const text = container.textContent ?? ''
    expect(text.indexOf('newer.example')).toBeLessThan(text.indexOf('older.example'))
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
    expect(screen.getByText('No self-issued tokens.')).toBeInTheDocument()
  })
})
