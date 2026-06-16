import { Link } from '@tanstack/react-router'
import { indexRoute } from '../router'
import { useProjectsQuery } from '../lib/queries'
import { namespacesOf } from '../lib/tree'
import { RecoverButton } from '../components/RecoverButton'
import styles from './RepoListPage.module.css'

export function RepoListPage() {
  // Typed search param ?ns= from the route's validateSearch.
  const { ns } = indexRoute.useSearch()
  const navigate = indexRoute.useNavigate()
  const { data: projects = [], isPending, isError, error } = useProjectsQuery()

  const namespaces = namespacesOf(projects)
  const filtered = ns ? projects.filter((p) => p.namespace === ns) : projects

  return (
    <div className={styles.page}>
      <header className={styles.head}>
        <h1 className={styles.title}>Projects</h1>
        <p className={styles.sub}>
          {isPending
            ? 'Loading…'
            : `${projects.length} project${
                projects.length === 1 ? '' : 's'
              } across ${namespaces.length} namespace${
                namespaces.length === 1 ? '' : 's'
              }`}
        </p>
      </header>

      {isError && (
        <div className={styles.noResults}>
          Could not load projects:{' '}
          {error instanceof Error ? error.message : 'unknown error'}
        </div>
      )}

      <div className={styles.filters}>
        <span className={styles.filterLabel}>Namespace</span>
        <button
          className={styles.chip}
          data-active={!ns}
          onClick={() => navigate({ search: {} })}
        >
          All
        </button>
        {namespaces.map((n) => (
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
                <span className={styles.stateDot} data-state={p.state} />
                {p.state}
              </div>
            </Link>
            {/* A non-healthy project gets an in-product recovery control (it sits
                outside the card <Link> so recovering never navigates). */}
            {p.state !== 'healthy' && (
              <RecoverButton namespace={p.namespace} project={p.name} />
            )}
          </li>
        ))}
      </ul>

      {!isPending && !isError && filtered.length === 0 && (
        <div className={styles.noResults}>
          {ns ? `No projects in “${ns}”.` : 'No projects yet.'}
        </div>
      )}
    </div>
  )
}
