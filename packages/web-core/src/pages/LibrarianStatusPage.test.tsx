import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

// Mock the imperative ops so the test asserts the rendered view + the refresh
// wiring, not the /ws/ui path.
const { librarianStatus, refreshLibrarianStatus, reloadLibrarianConfig, setLibrarianMaxSteps } = vi.hoisted(() => ({
  librarianStatus: vi.fn(),
  refreshLibrarianStatus: vi.fn(),
  reloadLibrarianConfig: vi.fn(),
  setLibrarianMaxSteps: vi.fn(),
}))
vi.mock('../lib/librarianStatus', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/librarianStatus')>()
  return { ...actual, librarianStatus, refreshLibrarianStatus, reloadLibrarianConfig, setLibrarianMaxSteps }
})

import { LibrarianStatusPage } from './LibrarianStatusPage'

function wrap(node: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={qc}>{node}</QueryClientProvider>
}

beforeEach(() => {
  librarianStatus.mockReset()
  refreshLibrarianStatus.mockReset()
  reloadLibrarianConfig.mockReset()
  setLibrarianMaxSteps.mockReset()
})

describe('LibrarianStatusPage', () => {
  it('shows the cached status (provider + model) on load', async () => {
    librarianStatus.mockResolvedValue({
      configured: true,
      provider: 'anthropic',
      model: 'claude-3-5-haiku-latest',
      kind: 'ready',
    })
    render(wrap(<LibrarianStatusPage />))

    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Ready'))
    expect(screen.getByText('anthropic')).toBeInTheDocument()
    expect(screen.getByText('claude-3-5-haiku-latest')).toBeInTheDocument()
    // The cached read does not trigger a refresh call.
    expect(refreshLibrarianStatus).not.toHaveBeenCalled()
  })

  it('re-runs the check when Refresh is clicked and shows the new status', async () => {
    librarianStatus.mockResolvedValue({ configured: true, provider: 'openai', model: 'gpt-x', kind: 'model_not_found', detail: 'the model "gpt-x" does not exist' })
    refreshLibrarianStatus.mockResolvedValue({ configured: true, provider: 'openai', model: 'gpt-4o', kind: 'ready' })

    render(wrap(<LibrarianStatusPage />))
    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Model not found'))
    expect(screen.getByTestId('librarian-detail')).toHaveTextContent('does not exist')

    await userEvent.click(screen.getByTestId('librarian-refresh'))

    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Ready'))
    expect(refreshLibrarianStatus).toHaveBeenCalledTimes(1)
  })

  it('reloads from the config file and shows the new model on success', async () => {
    librarianStatus.mockResolvedValue({ configured: true, provider: 'openai', model: 'gpt-4o', kind: 'ready' })
    reloadLibrarianConfig.mockResolvedValue({ configured: true, provider: 'gemini', model: 'gemini-2.5-flash', kind: 'ready' })

    render(wrap(<LibrarianStatusPage />))
    await waitFor(() => expect(screen.getByText('gpt-4o')).toBeInTheDocument())

    await userEvent.click(screen.getByTestId('librarian-reload'))

    await waitFor(() => expect(screen.getByText('gemini-2.5-flash')).toBeInTheDocument())
    expect(reloadLibrarianConfig).toHaveBeenCalledTimes(1)
  })

  it('keeps the previous status and shows the detail when a reload fails the connection test', async () => {
    librarianStatus.mockResolvedValue({ configured: true, provider: 'openai', model: 'gpt-4o', kind: 'ready' })
    reloadLibrarianConfig.mockResolvedValue({ configured: true, provider: 'openai', model: 'gpt-typo', kind: 'model_not_found', detail: 'the model "gpt-typo" does not exist' })

    render(wrap(<LibrarianStatusPage />))
    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Ready'))

    await userEvent.click(screen.getByTestId('librarian-reload'))

    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Model not found'))
    expect(screen.getByTestId('librarian-detail')).toHaveTextContent('does not exist')
    expect(reloadLibrarianConfig).toHaveBeenCalledTimes(1)
  })

  it('shows the max steps setting and saves a new value', async () => {
    librarianStatus.mockResolvedValue({
      configured: true,
      provider: 'openai',
      model: 'gpt-4o',
      kind: 'ready',
      maxSteps: 8,
    })
    setLibrarianMaxSteps.mockResolvedValue({
      configured: true,
      provider: 'openai',
      model: 'gpt-4o',
      kind: 'ready',
      maxSteps: 12,
    })
    render(wrap(<LibrarianStatusPage />))

    await waitFor(() => expect(screen.getByTestId('max-steps-input')).toHaveValue(8))

    const input = screen.getByTestId('max-steps-input')
    await userEvent.clear(input)
    await userEvent.type(input, '12')
    await userEvent.click(screen.getByTestId('max-steps-save'))

    await waitFor(() => expect(setLibrarianMaxSteps).toHaveBeenCalledWith(12))
  })

  it('never renders an API key', async () => {
    librarianStatus.mockResolvedValue({ configured: true, provider: 'anthropic', model: 'claude-x', kind: 'auth_failed', detail: 'the API key (environment variable) is missing or invalid' })
    render(wrap(<LibrarianStatusPage />))
    await waitFor(() => expect(screen.getByTestId('librarian-kind')).toHaveTextContent('Authentication failed'))
    // Sanity: the detail talks about the env var, not a secret value.
    expect(screen.queryByText(/sk-/)).not.toBeInTheDocument()
  })
})
