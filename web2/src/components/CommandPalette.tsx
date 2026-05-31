import { useEffect, useMemo, useState } from 'react'
import { Command } from 'cmdk'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { usePalette } from '../lib/palette'
import { useTheme } from '../lib/theme'
import { mockData, getProject, projectKey } from '../lib/data'
import { fuzzyFilter } from '../lib/fuzzy'
import styles from './CommandPalette.module.css'
import './cmdk.css'

// Parse the active project + file from the current path.
function useContextRefs() {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const pm = pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
  const fm = pathname.match(/^\/p\/[^/]+\/[^/]+\/(?:blob|edit)\/(.*)$/)
  return {
    pathname,
    ns: pm ? decodeURIComponent(pm[1]) : null,
    proj: pm ? decodeURIComponent(pm[2]) : null,
    filePath: fm ? fm[1] : null,
    inEdit: /\/edit\//.test(pathname),
  }
}

type Page = 'root' | 'files' | 'projects'

export function CommandPalette() {
  const { open, mode, setOpen, closePalette } = usePalette()
  const navigate = useNavigate()
  const { theme, toggle } = useTheme()
  const ctx = useContextRefs()

  const [page, setPage] = useState<Page>('root')
  const [search, setSearch] = useState('')

  // Reset page/search whenever the palette opens; honor the requested mode.
  useEffect(() => {
    if (open) {
      setSearch('')
      setPage(mode === 'files' ? 'files' : 'root')
    }
  }, [open, mode])

  const project = ctx.ns && ctx.proj ? getProject(ctx.ns, ctx.proj) : undefined

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
      // Open current file in editor: Cmd/Ctrl+E
      if (meta && !e.shiftKey && (e.key === 'e' || e.key === 'E')) {
        if (ctx.filePath && ctx.ns && ctx.proj && !ctx.inEdit) {
          e.preventDefault()
          navigate({
            to: '/p/$namespace/$project/edit/$',
            params: {
              namespace: ctx.ns,
              project: ctx.proj,
              _splat: ctx.filePath,
            },
          })
        }
        return
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [toggle, navigate, ctx.filePath, ctx.ns, ctx.proj, ctx.inEdit])

  const fileResults = useMemo(() => {
    if (!project) return []
    return fuzzyFilter(search, project.files, (f) => f.path).slice(0, 50)
  }, [project, search])

  const run = (fn: () => void) => {
    closePalette()
    // Defer so the dialog unmounts before navigation/alerts.
    setTimeout(fn, 0)
  }

  const copyDeepLink = () => {
    const url = window.location.href
    void navigator.clipboard?.writeText(url).catch(() => {})
  }

  return (
    <Command.Dialog
      open={open}
      onOpenChange={setOpen}
      label="Command palette"
      className={styles.dialog}
      shouldFilter={page === 'root' || page === 'projects'}
      onKeyDown={(e) => {
        // Backspace on empty search returns to root from a sub-page.
        if (e.key === 'Backspace' && search === '' && page !== 'root') {
          e.preventDefault()
          setPage('root')
        }
        if (e.key === 'Escape') {
          if (page !== 'root') {
            e.preventDefault()
            setPage('root')
            setSearch('')
          }
        }
      }}
    >
      <div className={styles.inputRow}>
        {page !== 'root' && (
          <span className={styles.crumb}>
            {page === 'files' ? 'Go to File' : 'Switch Project'}
          </span>
        )}
        <Command.Input
          autoFocus
          value={search}
          onValueChange={setSearch}
          placeholder={
            page === 'files'
              ? 'Type a file name…'
              : page === 'projects'
                ? 'Type a project…'
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
                disabled={!project}
                onSelect={() => {
                  setSearch('')
                  setPage('files')
                }}
              />
              {ctx.filePath && !ctx.inEdit && (
                <CmdItem
                  label="Open Current File in Editor"
                  kbd="⌘E"
                  onSelect={() =>
                    run(() =>
                      navigate({
                        to: '/p/$namespace/$project/edit/$',
                        params: {
                          namespace: ctx.ns!,
                          project: ctx.proj!,
                          _splat: ctx.filePath!,
                        },
                      }),
                    )
                  }
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
                label="Go to Repositories"
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
          (project ? (
            fileResults.map(({ item }) => (
              <Command.Item
                key={item.path}
                value={item.path}
                className={styles.item}
                onSelect={() =>
                  run(() =>
                    navigate({
                      to: '/p/$namespace/$project/blob/$',
                      params: {
                        namespace: ctx.ns!,
                        project: ctx.proj!,
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
                <span className={styles.itemPath}>{item.path}</span>
              </Command.Item>
            ))
          ) : (
            <div className={styles.empty}>Open a project first.</div>
          ))}

        {page === 'projects' &&
          mockData.projects.map((p) => (
            <Command.Item
              key={projectKey(p)}
              value={projectKey(p)}
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
  disabled,
  onSelect,
}: {
  label: string
  hint?: string
  kbd?: string
  disabled?: boolean
  onSelect: () => void
}) {
  return (
    <Command.Item
      value={label}
      disabled={disabled}
      onSelect={onSelect}
      className={styles.item}
    >
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
