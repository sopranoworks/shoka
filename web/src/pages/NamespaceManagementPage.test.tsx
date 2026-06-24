import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { ToastProvider } from '../lib/toast'

// Mock the ws ops + the super-user hook (vi.hoisted so the mock factory can reference them).
const { namespaceHealth, createNamespace, deleteNamespace, createProject, deleteProject, moveProject, renameProject, renameNamespace, namespaceRecover, isSuperUser } =
  vi.hoisted(() => ({
    namespaceHealth: vi.fn(),
    createNamespace: vi.fn(),
    deleteNamespace: vi.fn(),
    createProject: vi.fn(),
    deleteProject: vi.fn(),
    moveProject: vi.fn(),
    renameProject: vi.fn(),
    renameNamespace: vi.fn(),
    namespaceRecover: vi.fn(),
    isSuperUser: vi.fn(),
  }))
vi.mock('../lib/nsManageOps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/nsManageOps')>()
  return { ...actual, namespaceHealth, createNamespace, deleteNamespace, createProject, deleteProject, moveProject, renameProject, renameNamespace, namespaceRecover }
})
vi.mock('../lib/authStatus', () => ({ useIsSuperUser: isSuperUser }))

import { NamespaceManagementPage } from './NamespaceManagementPage'
import { CoreScreensProvider } from '../lib/coreScreens'
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
    // `default` is rename-protected: it must show NO namespace rename affordance, but a
    // project inside it renames normally.
    { name: 'default', present: true, healthy: true, projects: [{ name: 'home', state: 'healthy' }] },
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
    moveProject.mockReset()
    renameProject.mockReset()
    renameNamespace.mockReset()
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

  // #7 (move flow DISTINCT from delete) — the move control lives in its own slot, opens a
  // pick-target-namespace dropdown (NOT a type-name dialog), and calls move_project.
  it('move opens a pick-target dropdown distinct from delete and calls move_project', async () => {
    const user = userEvent.setup()
    moveProject.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    const foo = await screen.findByTestId('ns-foo')
    const alphaRow = within(foo).getByTestId('proj-foo-alpha')

    // The Move control is in the dedicated move-slot, separate from the Delete control.
    const moveSlot = within(alphaRow).getByTestId('move-slot-foo-alpha')
    const moveBtn = within(moveSlot).getByRole('button', { name: 'Move…' })
    const del = within(alphaRow).getByRole('button', { name: 'Delete' })
    expect(moveSlot.contains(del)).toBe(false)

    await user.click(moveBtn)
    const dialog = screen.getByRole('dialog', { name: /move project alpha/i })
    // It is a PICK-target dropdown, NOT a type-name-to-destroy dialog.
    const select = within(dialog).getByLabelText('target namespace')
    expect(within(dialog).queryByLabelText('confirm name')).toBeNull()

    // Confirm is disabled until a target is chosen; "empty" is the only other namespace.
    const confirm = within(dialog).getByRole('button', { name: 'Move' })
    expect(confirm).toBeDisabled()
    await user.selectOptions(select, 'empty')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(() => expect(moveProject).toHaveBeenCalledWith('foo', 'alpha', 'empty'))
  })

  it('hides the Move control from a non-super-user', async () => {
    renderPage(false)
    const foo = await screen.findByTestId('ns-foo')
    const moveSlot = within(foo).getByTestId('move-slot-foo-alpha')
    expect(within(moveSlot).queryByRole('button', { name: 'Move…' })).toBeNull()
  })

  // #9 (rename UI DISTINCT from move AND delete) — the project Rename control lives in its
  // own slot, opens a LOW-FRICTION edit-the-name dialog (NOT a type-to-destroy dialog, NOT a
  // pick-target dropdown), keeps Confirm disabled until the name is valid, changed, and
  // non-empty, and calls renameProject.
  it('project rename opens an edit-the-name dialog distinct from move and delete', async () => {
    const user = userEvent.setup()
    renameProject.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    const foo = await screen.findByTestId('ns-foo')
    const alphaRow = within(foo).getByTestId('proj-foo-alpha')

    // The Rename control is in its own dedicated slot, separate from the move slot and Delete.
    const renameSlot = within(alphaRow).getByTestId('rename-slot-foo-alpha')
    const renameBtn = within(renameSlot).getByRole('button', { name: 'Rename…' })
    const moveSlot = within(alphaRow).getByTestId('move-slot-foo-alpha')
    expect(renameSlot.contains(moveSlot)).toBe(false)

    await user.click(renameBtn)
    const dialog = screen.getByRole('dialog', { name: /rename project alpha/i })
    // NOT a type-to-destroy dialog, NOT a pick-target dropdown.
    expect(within(dialog).queryByLabelText('confirm name')).toBeNull()
    expect(within(dialog).queryByLabelText('target namespace')).toBeNull()

    const input = within(dialog).getByLabelText('New project name')
    const confirm = within(dialog).getByRole('button', { name: 'Rename' })
    // Pre-filled with the current name ⇒ unchanged ⇒ Confirm disabled.
    expect(input).toHaveValue('alpha')
    expect(confirm).toBeDisabled()
    // An invalid name keeps it disabled.
    await user.clear(input)
    await user.type(input, 'bad/name')
    expect(confirm).toBeDisabled()
    // A valid, changed name enables it.
    await user.clear(input)
    await user.type(input, 'alpha2')
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    await waitFor(() => expect(renameProject).toHaveBeenCalledWith('foo', 'alpha', 'alpha2'))
  })

  // #9 — namespace rename is super-user only and absent for the protected `default` namespace.
  it('namespace rename: super-user only, and never for default', async () => {
    const user = userEvent.setup()
    renameNamespace.mockResolvedValue({ status: 'ok' })
    renderPage(true)
    await screen.findByTestId('ns-foo')

    // `default` shows NO namespace rename affordance.
    expect(screen.queryByTestId('ns-rename-default')).toBeNull()
    // …but a project INSIDE default still renames (admin-on-ns).
    const def = screen.getByTestId('ns-default')
    expect(within(def).getByTestId('rename-slot-default-home')).toBeInTheDocument()

    // A normal namespace exposes Rename… for a super-user; it opens the edit-the-name dialog.
    const fooRename = screen.getByTestId('ns-rename-foo')
    await user.click(fooRename)
    const dialog = screen.getByRole('dialog', { name: /rename namespace foo/i })
    await user.clear(within(dialog).getByLabelText('New namespace name'))
    await user.type(within(dialog).getByLabelText('New namespace name'), 'foo2')
    await user.click(within(dialog).getByRole('button', { name: 'Rename' }))
    await waitFor(() => expect(renameNamespace).toHaveBeenCalledWith('foo', 'foo2'))
  })

  it('namespace rename control is hidden from a non-super-user; project rename is shown', async () => {
    renderPage(false) // a namespace-admin
    const foo = await screen.findByTestId('ns-foo')
    // No namespace-level rename control for a non-super-user.
    expect(screen.queryByTestId('ns-rename-foo')).toBeNull()
    // Project rename IS available (admin-on-ns — every listed namespace is administerable).
    const alphaRow = within(foo).getByTestId('proj-foo-alpha')
    expect(within(alphaRow).getByRole('button', { name: 'Rename…' })).toBeInTheDocument()
  })
})

// Layer-C extension seam: a consumer injects per-namespace and per-project sections via
// CoreScreensProvider. Default (no provider) renders nothing extra — covered implicitly by
// every test above (none of them sees these nodes).
describe('NamespaceManagementPage — injectable sections', () => {
  beforeEach(() => {
    isSuperUser.mockReset()
    namespaceHealth.mockReset()
    namespaceHealth.mockResolvedValue(report)
  })

  it('renders consumer-injected namespace- and project-level sections in place', async () => {
    isSuperUser.mockReturnValue(true)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <CoreScreensProvider
            value={{
              renderNamespaceSections: (ns) => <div data-testid={`ssh-${ns}`}>SSH keys for {ns}</div>,
              renderProjectSections: (ns, proj) => <span data-testid={`seed-${ns}-${proj}`}>seed</span>,
            }}
          >
            {children}
          </CoreScreensProvider>
        </ToastProvider>
      </QueryClientProvider>
    )
    render(<NamespaceManagementPage />, { wrapper })

    // Namespace-level section appears inside each namespace block.
    expect(await screen.findByTestId('ssh-foo')).toHaveTextContent('SSH keys for foo')
    expect(screen.getByTestId('ssh-empty')).toBeInTheDocument()
    // Project-level section appears as an extra row keyed to the project.
    expect(screen.getByTestId('proj-sections-foo-alpha')).toBeInTheDocument()
    expect(screen.getByTestId('seed-foo-alpha')).toHaveTextContent('seed')
  })
})
