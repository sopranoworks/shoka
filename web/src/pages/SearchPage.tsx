import { useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { searchRoute } from '../router'
import { useSearchQuery } from '../lib/search'
import { useDebouncedValue } from '../lib/useDebouncedValue'
import styles from './SearchPage.module.css'

// Project-scoped full-text/filename search. The URL's ?q= is the source of
// truth (deep-linkable, reload-safe, Back/Forward navigable); the input is a
// local mirror that, debounced, writes back to the URL. Results are {path,
// snippet}; clicking one opens the blob view (no scroll-to-line — the backend
// carries no line number).
export function SearchPage() {
  const { namespace, project } = searchRoute.useParams()
  const { q = '' } = searchRoute.useSearch()
  const navigate = useNavigate()

  const [term, setTerm] = useState(q)
  const debounced = useDebouncedValue(term, 250)

  // Keep the input in sync if the URL changes underneath us (Back/Forward,
  // deep-link, palette navigation).
  useEffect(() => {
    setTerm(q)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q])

  // Push the debounced term into the URL (replace, so typing doesn't spam
  // history). Only navigate when it actually differs from the current URL q.
  useEffect(() => {
    if (debounced === q) return
    void navigate({
      to: '/p/$namespace/$project/search',
      params: { namespace, project },
      search: debounced ? { q: debounced } : {},
      replace: true,
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced])

  const { data: matches, isFetching, isError } = useSearchQuery(
    namespace,
    project,
    q,
  )

  const hasQuery = q.trim() !== ''

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.scope} title={`${namespace}/${project}`}>
          Search <strong>{project}</strong>
        </span>
        <form
          className={styles.form}
          onSubmit={(e) => {
            e.preventDefault()
            void navigate({
              to: '/p/$namespace/$project/search',
              params: { namespace, project },
              search: term ? { q: term } : {},
              replace: true,
            })
          }}
        >
          <input
            autoFocus
            className={styles.input}
            type="search"
            value={term}
            onChange={(e) => setTerm(e.target.value)}
            placeholder="Search files in this project…"
            aria-label="Search query"
          />
        </form>
      </div>

      <div className={styles.body} data-scroll-restoration-id="search-body">
        {!hasQuery ? (
          <div className={styles.hint}>
            Type to search file names and contents in{' '}
            <code>{namespace}/{project}</code>.
          </div>
        ) : isError ? (
          <div className={styles.error}>Search failed. Try again.</div>
        ) : !matches ? (
          <div className={styles.hint}>Searching…</div>
        ) : matches.length === 0 ? (
          <div className={styles.hint}>
            No results for <code>{q}</code>.
          </div>
        ) : (
          <>
            <div className={styles.count}>
              {matches.length} {matches.length === 1 ? 'result' : 'results'}
              {isFetching && <span className={styles.fetching}> · updating…</span>}
            </div>
            <ul className={styles.results}>
              {matches.map((m) => (
                <li key={m.path}>
                  <button
                    className={styles.result}
                    onClick={() =>
                      void navigate({
                        to: '/p/$namespace/$project/blob/$',
                        params: { namespace, project, _splat: m.path },
                      })
                    }
                  >
                    <span className={styles.resultPath}>{m.path}</span>
                    {m.snippet && (
                      <span className={styles.snippet}>{m.snippet}</span>
                    )}
                  </button>
                </li>
              ))}
            </ul>
          </>
        )}
      </div>
    </div>
  )
}
