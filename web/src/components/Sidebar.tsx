import { useMemo, useState } from 'react'
import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from '../lib/useRailSelect'
import { FileTree, type TreeOpenMode } from './FileTree'
import { FileDropzone } from './FileDropzone'
import { SettingsItemList } from '@shoka/web-core'
import { useTreeQuery } from '../lib/queries'
import { filterTree, sortTree, type SortMode } from '../lib/tree'
import { dirOf } from '../lib/moveController'
import styles from './Sidebar.module.css'

// Pull the active namespace/project (if any) from the URL.
function useActiveProjectRef(): { ns: string; proj: string } | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
  if (!m) return null
  return { ns: decodeURIComponent(m[1]), proj: decodeURIComponent(m[2]) }
}

function useActiveFilePath(): string | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/[^/]+\/[^/]+\/blob\/(.*)$/)
  return m ? m[1] : null
}

// The file shown by the History route (so the History tree highlights it).
function useActiveHistoryPath(): string | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/[^/]+\/[^/]+\/history\/(.*)$/)
  return m && m[1] ? m[1] : null
}

export function Sidebar({ view }: { view: RailView }) {
  // All hooks unconditional, top-level, fixed order (Rules of Hooks — respect the
  // 1a370a4 #310 fix).
  const ref = useActiveProjectRef()
  const blobPath = useActiveFilePath()
  const historyPath = useActiveHistoryPath()

  if (view === 'search') return <SearchView projectRef={ref} />

  // Explorer, History AND Settings keep the SAME ProjectTree mounted at the SAME
  // position (only hidden in Settings), so switching among them never remounts the
  // tree — the file tree keeps its expansion state across the mode switch (74a7c8c,
  // extended to Settings: the tree is display:none in Settings, not unmounted). In
  // Settings the sidebar shows the permission-filtered settings-item list above the
  // (hidden) tree; on return to Explorer/History the tree is reconciled in place.
  const isHistory = view === 'history'
  const isSettings = view === 'settings'
  return (
    <>
      {isSettings && <SettingsItemList />}
      {ref ? (
        <div style={isSettings ? { display: 'none' } : { display: 'contents' }}>
          <ProjectTree
            ns={ref.ns}
            proj={ref.proj}
            activePath={isHistory ? historyPath : blobPath}
            openMode={isHistory ? 'history' : 'blob'}
          />
        </div>
      ) : (
        !isSettings && <div className={styles.pane} />
      )}
    </>
  )
}

// The Explorer and History rails both show the file tree (History keeps the tree
// in place — no separate cushioned route). They differ only in what a tree file
// opens: the file view, or that file's history (the right pane follows the
// selection). The header is identical so the tree reads the same in both modes.
function ProjectTree({
  ns,
  proj,
  activePath,
  openMode,
}: {
  ns: string
  proj: string
  activePath: string | null
  openMode: TreeOpenMode
}) {
  const { data: tree, isError } = useTreeQuery(ns, proj)
  const navigate = useNavigate()
  const [filter, setFilter] = useState('')
  const [sortMode, setSortMode] = useState<SortMode>('name-asc')
  const filteredTree = useMemo(
    () => (tree ? sortTree(filterTree(tree, filter), sortMode) : undefined),
    [tree, filter, sortMode],
  )
  const launchDir = activePath ? dirOf(activePath) : ''
  return (
    <div className={styles.pane}>
      <SectionHeader>
        <span className={styles.projTitle}>
          <span className={styles.projNs}>{ns}/</span>
          {proj}
        </span>
        {openMode !== 'history' && (
          <button
            type="button"
            className={styles.newFileBtn}
            title="New file"
            aria-label="New file"
            onClick={() =>
              void navigate({
                to: '/p/$namespace/$project/new',
                params: { namespace: ns, project: proj },
                search: launchDir ? { in: launchDir } : {},
              })
            }
          >
            + New file
          </button>
        )}
      </SectionHeader>
      <div className={styles.treeWrap}>
        <FileDropzone namespace={ns} project={proj}>
          <div className={styles.filterBar}>
            <div className={styles.filterInputWrap}>
              <input
                className={styles.filterInput}
                type="search"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
                placeholder="Filter files…"
                aria-label="Filter files by name"
              />
              {filter && (
                <button
                  className={styles.filterClear}
                  onClick={() => setFilter('')}
                  aria-label="Clear filter"
                  type="button"
                >
                  ×
                </button>
              )}
            </div>
            <select
              className={styles.sortSelect}
              value={sortMode}
              onChange={(e) => setSortMode(e.target.value as SortMode)}
              aria-label="Sort files"
            >
              <option value="name-asc">Name A→Z</option>
              <option value="name-desc">Name Z→A</option>
              <option value="date-desc">Newest</option>
              <option value="date-asc">Oldest</option>
            </select>
          </div>
          {isError ? (
            <div className={styles.empty}>Could not load files.</div>
          ) : !filteredTree ? (
            <div className={styles.empty}>Loading…</div>
          ) : filteredTree.length === 0 ? (
            <div className={styles.empty}>
              {filter ? 'No matching files.' : 'No files.'}
            </div>
          ) : (
            <FileTree
              namespace={ns}
              project={proj}
              nodes={filteredTree}
              activePath={activePath}
              openMode={openMode}
            />
          )}
        </FileDropzone>
      </div>
    </div>
  )
}

function SectionHeader({ children }: { children: React.ReactNode }) {
  return <div className={styles.sectionHeader}>{children}</div>
}

// Project-scoped full-text search. The form navigates to the search route,
// where the URL's ?q= is the source of truth; this sidebar input is just an
// entry point. Search needs a project in context (the backend searches one
// project at a time).
function SearchView({ projectRef }: { projectRef: { ns: string; proj: string } | null }) {
  const navigate = useNavigate()
  const [term, setTerm] = useState('')

  if (!projectRef) {
    return (
      <div className={styles.pane}>
        <SectionHeader>Search</SectionHeader>
        <div className={styles.empty}>
          Open a project to search its files.
          <br />
          <Link to="/" className={styles.emptyLink}>
            Choose a project →
          </Link>
        </div>
      </div>
    )
  }

  return (
    <div className={styles.pane}>
      <SectionHeader>Search</SectionHeader>
      <form
        className={styles.searchForm}
        onSubmit={(e) => {
          e.preventDefault()
          void navigate({
            to: '/p/$namespace/$project/search',
            params: { namespace: projectRef.ns, project: projectRef.proj },
            search: term.trim() ? { q: term.trim() } : {},
          })
        }}
      >
        <input
          className={styles.searchInput}
          type="search"
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          placeholder={`Search ${projectRef.proj}…`}
          aria-label="Search files"
        />
      </form>
      <div className={styles.empty}>
        Searches file names and contents in{' '}
        <code>{projectRef.ns}/{projectRef.proj}</code>. Press <kbd>⌘⇧F</kbd> from
        anywhere.
      </div>
    </div>
  )
}
