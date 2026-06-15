import { useState } from 'react'
import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from './ActivityRail'
import { FileTree, type TreeOpenMode } from './FileTree'
import { useTreeQuery } from '../lib/queries'
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
  const ref = useActiveProjectRef()
  if (view === 'search') return <SearchView projectRef={ref} />
  if (view === 'history') return <HistoryView />
  return <ExplorerView />
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
  // "+ New file" launches the create flow prefilled with the current location:
  // the directory of the open file (so the new file lands beside it), or the
  // project root when no file is open. The path stays editable to any nested
  // target (B-31 fix #3/#4) — this is the reach-it-from-anywhere affordance.
  const launchDir = activePath ? dirOf(activePath) : ''
  return (
    <div className={styles.pane}>
      <SectionHeader>
        <span className={styles.projTitle}>
          <span className={styles.projNs}>{ns}/</span>
          {proj}
        </span>
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
      </SectionHeader>
      <div className={styles.treeWrap}>
        {isError ? (
          <div className={styles.empty}>Could not load files.</div>
        ) : !tree ? (
          <div className={styles.empty}>Loading…</div>
        ) : tree.length === 0 ? (
          <div className={styles.empty}>No files.</div>
        ) : (
          <FileTree
            namespace={ns}
            project={proj}
            nodes={tree}
            activePath={activePath}
            openMode={openMode}
          />
        )}
      </div>
    </div>
  )
}

function SectionHeader({ children }: { children: React.ReactNode }) {
  return <div className={styles.sectionHeader}>{children}</div>
}

function ExplorerView() {
  const ref = useActiveProjectRef()
  const activePath = useActiveFilePath()

  if (!ref) {
    return (
      <div className={styles.pane}>
        <SectionHeader>Explorer</SectionHeader>
        <div className={styles.empty}>
          No project open.
          <br />
          <Link to="/" className={styles.emptyLink}>
            Choose a project →
          </Link>
        </div>
      </div>
    )
  }
  return (
    <ProjectTree
      ns={ref.ns}
      proj={ref.proj}
      activePath={activePath}
      openMode="blob"
    />
  )
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

// History mode keeps the file tree in place (no separate cushioned route): the
// rail toggles the right pane to the selected file's history, and the tree here
// opens each file's history (openMode="history") so selecting another file makes
// the right pane follow it. The full commit list / version / diff render in the
// content pane on the history route; when no file is selected that pane shows a
// quiet placeholder.
function HistoryView() {
  const ref = useActiveProjectRef()
  const activePath = useActiveHistoryPath()

  if (!ref) {
    return (
      <div className={styles.pane}>
        <SectionHeader>History</SectionHeader>
        <div className={styles.empty}>
          Open a project to view file history.
          <br />
          <Link to="/" className={styles.emptyLink}>
            Choose a project →
          </Link>
        </div>
      </div>
    )
  }

  return (
    <ProjectTree
      ns={ref.ns}
      proj={ref.proj}
      activePath={activePath}
      openMode="history"
    />
  )
}
