import { useEffect, useMemo, useState } from 'react'
import { Command } from 'cmdk'
import { useNavigate } from '@tanstack/react-router'
import { usePalette } from '../lib/palette'
import { useTheme } from '../lib/theme'
import { useProjectsQuery, useAllProjectFiles } from '../lib/queries'
import { namespacesOf } from '../lib/tree'
import { fuzzyFilter } from '../lib/fuzzy'
import styles from './CommandPalette.module.css'
import './cmdk.css'

type Page = 'root' | 'files' | 'projects' | 'namespaces'

export function CommandPalette() {
  const { open, mode, setOpen, closePalette } = usePalette()
  const navigate = useNavigate()
  const { theme, toggle } = useTheme()

  const [page, setPage] = useState<Page>('root')
  const [search, setSearch] = useState('')

  // Reset page/search whenever the palette opens; honor the requested mode.
  useEffect(() => {
    if (open) {
      setSearch('')
      setPage(mode === 'files' ? 'files' : 'root')
    }
  }, [open, mode])

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
