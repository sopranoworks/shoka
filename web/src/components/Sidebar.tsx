import { useMemo, useState } from 'react'
import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from './ActivityRail'
import { FileTree } from './FileTree'
import { useProjectsQuery, useTreeQuery } from '../lib/queries'
import type { ProjectInfo } from '../lib/types'
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

export function Sidebar({ view }: { view: RailView }) {
  const ref = useActiveProjectRef()
  if (view === 'namespaces') return <NamespacesView />
  if (view === 'search') return <SearchView projectRef={ref} />
  if (view === 'history') return <HistoryView />
  return <ExplorerView />
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
            Choose a repository →
          </Link>
        </div>
      </div>
    )
  }
  return <ExplorerForProject ns={ref.ns} proj={ref.proj} activePath={activePath} />
}

function ExplorerForProject({
  ns,
  proj,
  activePath,
}: {
  ns: string
  proj: string
  activePath: string | null
}) {
  const { data: tree, isError } = useTreeQuery(ns, proj)
  return (
    <div className={styles.pane}>
      <SectionHeader>
        <span className={styles.projTitle}>
          <span className={styles.projNs}>{ns}/</span>
          {proj}
        </span>
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
          />
        )}
      </div>
    </div>
  )
}

function NamespacesView() {
  const { data: projects = [] } = useProjectsQuery()
  const grouped = useMemo(() => {
    const map = new Map<string, ProjectInfo[]>()
    for (const p of projects) {
      if (!map.has(p.namespace)) map.set(p.namespace, [])
      map.get(p.namespace)!.push(p)
    }
    return [...map.entries()].sort((a, b) => a[0].localeCompare(b[0]))
  }, [projects])

  return (
    <div className={styles.pane}>
      <SectionHeader>Namespaces</SectionHeader>
      <div className={styles.list}>
        {grouped.map(([ns, nsProjects]) => (
          <div key={ns} className={styles.nsGroup}>
            <Link to="/" search={{ ns }} className={styles.nsLink}>
              {ns}
              <span className={styles.count}>{nsProjects.length}</span>
            </Link>
            {nsProjects.map((p) => (
              <Link
                key={p.name}
                to="/p/$namespace/$project"
                params={{ namespace: p.namespace, project: p.name }}
                className={styles.nsProject}
              >
                {p.name}
              </Link>
            ))}
          </div>
        ))}
      </div>
    </div>
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
            Choose a repository →
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

function HistoryView() {
  return (
    <div className={styles.pane}>
      <SectionHeader>History</SectionHeader>
      <div className={styles.empty}>Commit history lands in a later session.</div>
    </div>
  )
}
