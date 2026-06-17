import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { ToastProvider } from '../lib/toast'

// Mock the ws ops + the super-user hook (vi.hoisted so the mock factory can reference them).
const { namespaceHealth, createNamespace, deleteNamespace, createProject, deleteProject, namespaceRecover, isSuperUser } =
  vi.hoisted(() => ({
    namespaceHealth: vi.fn(),
    createNamespace: vi.fn(),
    deleteNamespace: vi.fn(),
    createProject: vi.fn(),
    deleteProject: vi.fn(),
    namespaceRecover: vi.fn(),
    isSuperUser: vi.fn(),
  }))
vi.mock('../lib/nsManageOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/nsManageOps')>()
  return { ...actual, namespaceHealth, createNamespace, deleteNamespace, createProject, deleteProject, namespaceRecover }
})
vi.mock('../lib/authStatus', () => ({ useIsSuperUser: isSuperUser }))

import { NamespaceManagementPage } from './NamespaceManagementPage'
import type { HealthReport } from '../lib/nsManageOps'

const report: HealthReport = {
  namespaces: [
    {
      name: 'foo',
      present: true,
      healthy: false,
      projects: [
        { name: 'alpha', state: 'healthy' },
        { name: 'beta', state: 'corrupted' },
        { name: 'ghost', state: 'missing' },
      ],
    },
    { name: 'empty', present: true, healthy: true, projects: [] },
  ],
  foreign_namespaces: [{ name: 'stray', adoptable: true }],
}

function renderPage(superUser: boolean) {
  isSuperUser.mockReturnValue(superUser)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <ToastProvider>{children}</ToastProvider>
    </QueryClientProvider>
  )
  return render(<NamespaceManagementPage />, { wrapper })
}

describe('NamespaceManagementPage', () => {
  beforeEach(() => {
    namespaceHealth.mockReset()
    createNamespace.mockReset()
    deleteNamespace.mockReset()
    createProject.mockReset()
    deleteProject.mockReset()
    namespaceRecover.mockReset()
    isSuperUser.mockReset()
    namespaceHealth.mockResolvedValue(report)
  })

  // #3 — listing + health badges.
  it('renders namespaces, projects, and per-project health badges', async () => {
    renderPage(true)
    expect(await screen.findByTestId('ns-foo')).toBeInTheDocument()
    const foo = screen.getByTestId('ns-foo')
    expect(within(foo).getByText('alpha')).toBeInTheDocument()
    expect(within(foo).getByText('healthy')).toBeInTheDocument()
    expect(within(foo).getByText('corrupted')).toBeInTheDocument()
    expect(within(foo).getByText('missing')).toBeInTheDocument()
  })

  // #4 — add gated: namespace add = super-user only; project add = shown for a listed
  // (administerable) namespace either way; create_* called.
  it('shows add-namespace only for a super-user; add-project for any listed namespace', async () => {
    renderPage(false) // a namespace-admin (not super-user)
    await screen.findByTestId('ns-foo')
    expect(screen.queryByRole('button', { name: '+ New namespace' })).toBeNull()
    expect(screen.getAllByRole('button', { name: '+ Add project' }).length).toBeGreaterThan(0)
  })

  it('super-user can add a namespace and a project', async () => {
    const user = userEvent.setup()
    createNamespace.mockResolvedValue({ status: 'ok' })
    createProject.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    await screen.findByTestId('ns-foo')

    await user.click(screen.getByRole('button', { name: '+ New namespace' }))
    await user.type(screen.getByLabelText('Namespace name'), 'newns')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() => expect(createNamespace).toHaveBeenCalledWith('newns'))

    await user.click(within(screen.getByTestId('ns-foo')).getByRole('button', { name: '+ Add project' }))
    await user.type(screen.getByLabelText('Project name'), 'newp')
    await user.click(screen.getByRole('button', { name: 'Create' }))
    await waitFor(() => expect(createProject).toHaveBeenCalledWith('foo', 'newp'))
  })

  // #5 (core) — project delete is high-friction: Confirm stays disabled until the EXACT
  // project name is typed, then calls DeleteProject.
  it('project delete requires typing the exact name before Confirm enables', async () => {
    const user = userEvent.setup()
    deleteProject.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    const foo = await screen.findByTestId('ns-foo')

    // Delete the healthy project "alpha".
    const alphaRow = within(foo).getByTestId('proj-foo-alpha')
    await user.click(within(alphaRow).getByRole('button', { name: 'Delete' }))

    const dialog = screen.getByRole('dialog')
    const confirm = within(dialog).getByRole('button', { name: 'Delete' })
    const input = within(dialog).getByLabelText('confirm name')
    expect(confirm).toBeDisabled()
    await user.type(input, 'wrong')
    expect(confirm).toBeDisabled()
    await user.clear(input)
    await user.type(input, 'alpha')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(() => expect(deleteProject).toHaveBeenCalledWith('foo', 'alpha'))
  })

  // #5 (core) — namespace delete is super-user + EMPTY-ONLY: disabled while it has
  // projects (with a reason), enabled + type-confirm when empty.
  it('namespace delete is disabled while non-empty and type-confirmed when empty', async () => {
    const user = userEvent.setup()
    deleteNamespace.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    await screen.findByTestId('ns-foo')

    // foo has projects → its Delete namespace button is disabled with the reason.
    const fooDelete = within(screen.getByTestId('ns-foo')).getByRole('button', { name: 'Delete namespace' })
    expect(fooDelete).toBeDisabled()
    expect(fooDelete).toHaveAttribute('title', expect.stringMatching(/delete its projects first/i))

    // empty namespace → enabled → type-confirm.
    const emptyDelete = within(screen.getByTestId('ns-empty')).getByRole('button', { name: 'Delete namespace' })
    expect(emptyDelete).toBeEnabled()
    await user.click(emptyDelete)
    const dialog = screen.getByRole('dialog')
    const confirm = within(dialog).getByRole('button', { name: 'Delete' })
    expect(confirm).toBeDisabled()
    await user.type(within(dialog).getByLabelText('confirm name'), 'empty')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(() => expect(deleteNamespace).toHaveBeenCalledWith('empty'))
  })

  // #6 — future-move room: each project row has a dedicated move slot distinct from Delete.
  it('leaves a distinct move-action slot on each project row, separate from delete', async () => {
    renderPage(true)
    const foo = await screen.findByTestId('ns-foo')
    const alphaRow = within(foo).getByTestId('proj-foo-alpha')
    const moveSlot = within(alphaRow).getByTestId('move-slot-foo-alpha')
    expect(moveSlot).toBeInTheDocument()
    // The slot is a separate element from the Delete control (never confusable).
    const del = within(alphaRow).getByRole('button', { name: 'Delete' })
    expect(moveSlot).not.toBe(del)
    expect(moveSlot.contains(del)).toBe(false)
  })
})
