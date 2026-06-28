import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AuthGate } from './AuthGate'
import * as authClient from '@shoka/web-core'

// wsClient opens a real socket on connect(); stub it so AuthGate's "enter the app"
// branch does not try to open /ws/ui in jsdom.
vi.mock('@shoka/web-core', async (importOriginal) => ({ ...(await importOriginal<typeof import('@shoka/web-core')>()), wsClient: () => ({ connect: vi.fn() }) }))

function mockStatus(s: Partial<authClient.AuthStatus>) {
  vi.spyOn(authClient, 'getStatus').mockResolvedValue({
    users_exist: false,
    authenticated: false,
    first_run_allowed: true,
    passkey_enabled: false,
    ...s,
  })
}

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>)
}

describe('AuthGate', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('shows the first-run wizard when no users exist and first-run is allowed', async () => {
    mockStatus({ users_exist: false, first_run_allowed: true })
    renderWithQuery(
      <AuthGate>
        <div>APP CONTENT</div>
      </AuthGate>,
    )
    expect(await screen.findByLabelText('First-run setup')).toBeInTheDocument()
    expect(screen.queryByText('APP CONTENT')).not.toBeInTheDocument()
  })

  it('shows the login screen when users exist but the request is not authenticated', async () => {
    mockStatus({ users_exist: true, authenticated: false })
    renderWithQuery(
      <AuthGate>
        <div>APP CONTENT</div>
      </AuthGate>,
    )
    expect(await screen.findByLabelText('Sign in')).toBeInTheDocument()
    expect(screen.queryByText('APP CONTENT')).not.toBeInTheDocument()
  })

  it('renders the app (viewing under the session) once authenticated', async () => {
    mockStatus({ users_exist: true, authenticated: true, principal: { email: 'op@x.com', display_name: 'Op', is_admin: true } })
    renderWithQuery(
      <AuthGate>
        <div>APP CONTENT</div>
      </AuthGate>,
    )
    expect(await screen.findByText('APP CONTENT')).toBeInTheDocument()
  })

  it('renders the app without login when no users exist and first-run is disabled (no-lockout)', async () => {
    mockStatus({ users_exist: false, first_run_allowed: false })
    renderWithQuery(
      <AuthGate>
        <div>APP CONTENT</div>
      </AuthGate>,
    )
    expect(await screen.findByText('APP CONTENT')).toBeInTheDocument()
  })
})
