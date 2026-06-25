import { useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate as useNav, useRouterState } from '@tanstack/react-router'
import { FileTree, type TreeOpenMode } from './FileTree'
import { SettingsItemList } from './SettingsItemList'
import { useTreeQuery } from '../lib/queries'
import { filterTree, sortTree, dirOf, type SortMode } from '../lib/tree'
import { useContentConfig } from '../lib/contentConfig'
import styles from './Sidebar.module.css'

export { styles as sidebarStyles }

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

function useActiveHistoryPath(): string | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/[^/]+\/[^/]+\/history\/(.*)$/)
  return m && m[1] ? m[1] : null
}

export function Sidebar({ view }: { view: string }) {
  const ref = useActiveProjectRef()
  const blobPath = useActiveFilePath()
  const historyPath = useActiveHistoryPath()

  if (view === 'search') return <SearchView projectRef={ref} />

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
  const [filter, setFilter] = useState('')
  const [sortMode, setSortMode] = useState<SortMode>('name-asc')
  const filteredTree = useMemo(
    () => (tree ? sortTree(filterTree(tree, filter), sortMode) : undefined),
    [tree, filter, sortMode],
  )
  const { renderNewFileButton, renderDropZone } = useContentConfig()
  const launchDir = activePath ? dirOf(activePath) : ''

  const treeContent = (
    <>
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
    </>
  )

  return (
    <div className={styles.pane}>
      <SectionHeader>
        <span className={styles.projTitle}>
          <span className={styles.projNs}>{ns}/</span>
          {proj}
        </span>
        {openMode !== 'history' && renderNewFileButton && (
          renderNewFileButton(ns, proj, launchDir)
        )}
      </SectionHeader>
      <div className={styles.treeWrap}>
        {renderDropZone
          ? renderDropZone(ns, proj, treeContent)
          : treeContent}
      </div>
    </div>
  )
}

function SectionHeader({ children }: { children: ReactNode }) {
  return <div className={styles.sectionHeader}>{children}</div>
}

function SearchView({ projectRef }: { projectRef: { ns: string; proj: string } | null }) {
  const navigate = useNav()
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
