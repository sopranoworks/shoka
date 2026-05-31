import { Link } from '@tanstack/react-router'
import { indexRoute } from '../router'
import { mockData } from '../lib/data'
import { useProjectsQuery } from '../lib/queries'
import styles from './RepoListPage.module.css'

export function RepoListPage() {
  // Typed search param ?ns= from the route's validateSearch.
  const { ns } = indexRoute.useSearch()
  const navigate = indexRoute.useNavigate()
  const { data: projects = [] } = useProjectsQuery()

  const filtered = ns ? projects.filter((p) => p.namespace === ns) : projects

  return (
    <div className={styles.page}>
      <header className={styles.head}>
        <h1 className={styles.title}>Repositories</h1>
        <p className={styles.sub}>
          {projects.length} projects across {mockData.namespaces.length} namespaces
        </p>
      </header>

      <div className={styles.filters}>
        <span className={styles.filterLabel}>Namespace</span>
        <button
          className={styles.chip}
          data-active={!ns}
          onClick={() => navigate({ search: {} })}
        >
          All
        </button>
        {mockData.namespaces.map((n) => (
          <button
            key={n}
            className={styles.chip}
            data-active={ns === n}
            onClick={() => navigate({ search: { ns: n } })}
          >
            {n}
          </button>
        ))}
      </div>

      <ul className={styles.grid}>
        {filtered.map((p) => (
          <li key={`${p.namespace}/${p.name}`}>
            {/* Real <Link> anchor: middle-click / open-in-new-tab works. */}
            <Link
              to="/p/$namespace/$project"
              params={{ namespace: p.namespace, project: p.name }}
              className={styles.card}
            >
              <div className={styles.cardTop}>
                <span className={styles.repoIcon}>
                  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                    <path
                      d="M3 2.5h8a1.5 1.5 0 0 1 1.5 1.5v9.5H4.5A1.5 1.5 0 0 1 3 12z"
                      stroke="currentColor"
                      strokeWidth="1.3"
                    />
                  </svg>
                </span>
                <span className={styles.cardName}>{p.name}</span>
              </div>
              <div className={styles.cardNs}>{p.namespace}</div>
              <div className={styles.cardMeta}>
                {p.files.length} file{p.files.length === 1 ? '' : 's'}
              </div>
            </Link>
          </li>
        ))}
      </ul>

      {filtered.length === 0 && (
        <div className={styles.noResults}>No projects in “{ns}”.</div>
      )}
    </div>
  )
}
