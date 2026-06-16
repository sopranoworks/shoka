import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Command } from 'cmdk'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { usePalette } from '../lib/palette'
import { useMoveController, dirOf } from '../lib/moveController'
import { useTheme } from '../lib/theme'
import { useIsAdmin } from '../lib/admin'
import { useProjectsQuery, useAllProjectFiles } from '../lib/queries'
import { namespacesOf } from '../lib/tree'
import { deriveViewContext } from '../lib/viewContext'
import { fuzzyFilter } from '../lib/fuzzy'
import styles from './CommandPalette.module.css'
import './cmdk.css'

type Page = 'root' | 'files' | 'projects' | 'namespaces'

export function CommandPalette() {
  const { open, mode, setOpen, closePalette, openPalette } = usePalette()
  const navigate = useNavigate()
  const { requestMove } = useMoveController()
  const { theme, toggle } = useTheme()
  const isAdmin = useIsAdmin()

  const [page, setPage] = useState<Page>('root')
  const [search, setSearch] = useState('')

  // Reset page/search whenever the palette opens; honor the requested mode.
  useEffect(() => {
    if (open) {
      setSearch('')
      setPage(mode === 'files' ? 'files' : 'root')
    }
  }, [open, mode])

  // Current view: the "Edit current file" command + ⌘E are only meaningful on a
  // file view (blob route).
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const view = useMemo(() => deriveViewContext(pathname), [pathname])
  const onBlob = view.route === 'blob' && !!view.namespace && !!view.project
  const onProject = !!view.namespace && !!view.project
  // Move/Rename operate on the file in context — the open blob OR edit file.
  const onFileView =
    (view.route === 'blob' || view.route === 'edit') &&
    !!view.namespace &&
    !!view.project &&
    !!view.path

  const editCurrentFile = useCallback(() => {
    if (view.route !== 'blob' || !view.namespace || !view.project) return
    void navigate({
      to: '/p/$namespace/$project/edit/$',
      params: {
        namespace: view.namespace,
        project: view.project,
        _splat: view.path ?? '',
      },
    })
  }, [view, navigate])
  const editCurrentRef = useRef(editCurrentFile)
  editCurrentRef.current = editCurrentFile

  // Search the project in context (the backend searches one project at a time).
  // When there is no project in context, fall back to opening the palette so the
  // user can switch to one first.
  const searchProject = useCallback(() => {
    if (!view.namespace || !view.project) {
      openPalette('commands')
      return
    }
    void navigate({
      to: '/p/$namespace/$project/search',
      params: { namespace: view.namespace, project: view.project },
    })
  }, [view, navigate, openPalette])
  const searchProjectRef = useRef(searchProject)
  searchProjectRef.current = searchProject

  // New file in the project in context (path chosen at Save time). When a file is
  // open, carry its directory so the create dialog prefills that location
  // (B-31 fix #3); the path stays editable to any nested target.
  const newFileInProject = useCallback(() => {
    if (!view.namespace || !view.project) return
    const launchDir = view.path ? dirOf(view.path) : ''
    void navigate({
      to: '/p/$namespace/$project/new',
      params: { namespace: view.namespace, project: view.project },
      search: launchDir ? { in: launchDir } : {},
    })
  }, [view, navigate])

  // Move / Rename the file in context. These open the move/rename dialog (the
  // operator's primary surface); the move is a pure path change, so the dialog
  // goes straight to the move with no link interstitial.
  const moveCurrentFile = useCallback(() => {
    if (
      (view.route !== 'blob' && view.route !== 'edit') ||
      !view.namespace ||
      !view.project ||
      !view.path
    )
      return
    requestMove({
      namespace: view.namespace,
      project: view.project,
      sourcePath: view.path,
      mode: 'move',
    })
  }, [view, requestMove])
  const renameCurrentFile = useCallback(() => {
    if (
      (view.route !== 'blob' && view.route !== 'edit') ||
      !view.namespace ||
      !view.project ||
      !view.path
    )
      return
    requestMove({
      namespace: view.namespace,
      project: view.project,
      sourcePath: view.path,
      mode: 'rename',
    })
  }, [view, requestMove])

  const { data: projects = [] } = useProjectsQuery()
  const namespaces = useMemo(() => namespacesOf(projects), [projects])
  // Global quick-open: load every project's files, but only while that page is
  // open (lazy — the N GET_TREE calls share the sidebar's ['tree'] cache).
  const allFiles = useAllProjectFiles(open && page === 'files')

  // Global shortcuts shown inline in the palette, wired for real.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const meta = e.metaKey || e.ctrlKey
      // Toggle theme: Cmd/Ctrl+Shift+L
      if (meta && e.shiftKey && (e.key === 'l' || e.key === 'L')) {
        e.preventDefault()
        toggle()
        return
      }
      // Copy deep-link: Cmd/Ctrl+Shift+C
      if (meta && e.shiftKey && (e.key === 'c' || e.key === 'C')) {
        e.preventDefault()
        void navigator.clipboard?.writeText(window.location.href).catch(() => {})
        return
      }
      // Edit current file: Cmd/Ctrl+E (only acts on a file view).
      if (meta && !e.shiftKey && (e.key === 'e' || e.key === 'E')) {
        e.preventDefault()
        editCurrentRef.current()
        return
      }
      // Search files: Cmd/Ctrl+Shift+F.
      if (meta && e.shiftKey && (e.key === 'f' || e.key === 'F')) {
        e.preventDefault()
        searchProjectRef.current()
        return
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [toggle])

  const fileResults = useMemo(
    () => fuzzyFilter(search, allFiles, (f) => f.path).slice(0, 50),
    [allFiles, search],
  )

  const run = (fn: () => void) => {
    closePalette()
    // Defer so the dialog unmounts before navigation.
    setTimeout(fn, 0)
  }

  const copyDeepLink = () => {
    void navigator.clipboard?.writeText(window.location.href).catch(() => {})
  }

  const crumbLabel =
    page === 'files'
      ? 'Go to File'
      : page === 'projects'
        ? 'Switch Project'
        : page === 'namespaces'
          ? 'Switch Namespace'
          : ''

  return (
    <Command.Dialog
      open={open}
      onOpenChange={setOpen}
      label="Command palette"
      className={styles.dialog}
      shouldFilter={page !== 'files'}
      onKeyDown={(e) => {
        // Backspace on empty search, or Escape, returns to root from a sub-page.
        if (e.key === 'Backspace' && search === '' && page !== 'root') {
          e.preventDefault()
          setPage('root')
        }
        if (e.key === 'Escape' && page !== 'root') {
          e.preventDefault()
          setPage('root')
          setSearch('')
        }
      }}
    >
      <div className={styles.inputRow}>
        {page !== 'root' && <span className={styles.crumb}>{crumbLabel}</span>}
        <Command.Input
          autoFocus
          value={search}
          onValueChange={setSearch}
          placeholder={
            page === 'files'
              ? 'Type a file name…'
              : page === 'projects'
                ? 'Type a project…'
                : page === 'namespaces'
                  ? 'Type a namespace…'
                  : 'Type a command or search…'
          }
          className={styles.input}
        />
      </div>

      <Command.List className={styles.list}>
        <Command.Empty className={styles.empty}>No results.</Command.Empty>

        {page === 'root' && (
          <>
            <Command.Group heading="File" className={styles.group}>
              <CmdItem
                label="Go to File…"
                hint="Quick-open by name"
                kbd="⌘P"
                onSelect={() => {
                  setSearch('')
                  setPage('files')
                }}
              />
              {onBlob && (
                <CmdItem
                  label="Edit current file"
                  kbd="⌘E"
                  onSelect={() => run(editCurrentFile)}
                />
              )}
              {onProject && (
                <CmdItem
                  label="Search files"
                  hint="In this project"
                  kbd="⌘⇧F"
                  onSelect={() => run(searchProject)}
                />
              )}
              {onProject && (
                <CmdItem
                  label="New file in this project"
                  hint="Choose a path at save"
                  onSelect={() => run(newFileInProject)}
                />
              )}
              {onFileView && (
                <CmdItem
                  label="Rename file…"
                  hint="Same folder, new name"
                  onSelect={() => run(renameCurrentFile)}
                />
              )}
              {onFileView && (
                <CmdItem
                  label="Move file…"
                  hint="Type the destination path"
                  onSelect={() => run(moveCurrentFile)}
                />
              )}
            </Command.Group>

            <Command.Group heading="Navigation" className={styles.group}>
              <CmdItem
                label="Switch Project…"
                kbd="⌃⌘P"
                onSelect={() => {
                  setSearch('')
                  setPage('projects')
                }}
              />
              <CmdItem
                label="Switch Namespace…"
                onSelect={() => {
                  setSearch('')
                  setPage('namespaces')
                }}
              />
              <CmdItem
                label="Go Home"
                onSelect={() => run(() => navigate({ to: '/' }))}
              />
            </Command.Group>

            {/* Admin group — exposed only when the admin predicate is true. This
                UI gate is SECONDARY; the authoritative gate is the server-side
                admin check on OAUTH_LIST/OAUTH_REVOKE (manager.go §2.1a). */}
            {isAdmin && (
              <Command.Group heading="Admin" className={styles.group}>
                <CmdItem
                  label="Manage OAuth connections…"
                  hint="List & revoke MCP connections"
                  onSelect={() =>
                    run(() => navigate({ to: '/settings', search: { item: 'oauth' } }))
                  }
                />
              </Command.Group>
            )}

            <Command.Group heading="View" className={styles.group}>
              <CmdItem
                label={`Toggle Theme (now: ${theme})`}
                kbd="⌘⇧L"
                onSelect={() => run(toggle)}
              />
              <CmdItem
                label="Copy Deep-link to Current View"
                kbd="⌘⇧C"
                onSelect={() => run(copyDeepLink)}
              />
            </Command.Group>
          </>
        )}

        {page === 'files' &&
          fileResults.map(({ item }) => (
            <Command.Item
              key={`${item.namespace}/${item.project}/${item.path}`}
              value={`${item.namespace}/${item.project}/${item.path}`}
              className={styles.item}
              onSelect={() =>
                run(() =>
                  navigate({
                    to: '/p/$namespace/$project/blob/$',
                    params: {
                      namespace: item.namespace,
                      project: item.project,
                      _splat: item.path,
                    },
                  }),
                )
              }
            >
              <FileGlyph />
              <span className={styles.itemLabel}>
                {item.path.split('/').pop()}
              </span>
              <span className={styles.itemPath}>
                {item.namespace}/{item.project}/{item.path}
              </span>
            </Command.Item>
          ))}

        {page === 'projects' &&
          projects.map((p) => (
            <Command.Item
              key={`${p.namespace}/${p.name}`}
              value={`${p.namespace}/${p.name}`}
              className={styles.item}
              onSelect={() =>
                run(() =>
                  navigate({
                    to: '/p/$namespace/$project',
                    params: { namespace: p.namespace, project: p.name },
                  }),
                )
              }
            >
              <RepoGlyph />
              <span className={styles.itemLabel}>{p.name}</span>
              <span className={styles.itemPath}>{p.namespace}</span>
            </Command.Item>
          ))}

        {page === 'namespaces' &&
          namespaces.map((n) => (
            <Command.Item
              key={n}
              value={n}
              className={styles.item}
              onSelect={() =>
                run(() => navigate({ to: '/', search: { ns: n } }))
              }
            >
              <RepoGlyph />
              <span className={styles.itemLabel}>{n}</span>
            </Command.Item>
          ))}
      </Command.List>

      <div className={styles.footer}>
        <span>
          <kbd>↑↓</kbd> navigate
        </span>
        <span>
          <kbd>↵</kbd> select
        </span>
        <span>
          <kbd>esc</kbd> {page === 'root' ? 'close' : 'back'}
        </span>
      </div>
    </Command.Dialog>
  )
}

function CmdItem({
  label,
  hint,
  kbd,
  onSelect,
}: {
  label: string
  hint?: string
  kbd?: string
  onSelect: () => void
}) {
  return (
    <Command.Item value={label} onSelect={onSelect} className={styles.item}>
      <span className={styles.itemLabel}>{label}</span>
      {hint && <span className={styles.itemHint}>{hint}</span>}
      {kbd && <KbdGroup combo={kbd} />}
    </Command.Item>
  )
}

// Render a keybinding string like "⌘⇧L" as individual <kbd> chips (VS Code style).
function KbdGroup({ combo }: { combo: string }) {
  const keys = [...combo]
  return (
    <span className={styles.kbdGroup}>
      {keys.map((k, i) => (
        <kbd key={i}>{k}</kbd>
      ))}
    </span>
  )
}

function FileGlyph() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" className={styles.glyph}>
      <path d="M4 2h5l3 3v9H4z" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
      <path d="M9 2v3h3" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
    </svg>
  )
}
function RepoGlyph() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" className={styles.glyph}>
      <path d="M3 2.5h8a1.5 1.5 0 0 1 1.5 1.5v9.5H4.5A1.5 1.5 0 0 1 3 12z" stroke="currentColor" strokeWidth="1.2" />
      <path d="M4.5 11.5h8" stroke="currentColor" strokeWidth="1.2" />
    </svg>
  )
}
