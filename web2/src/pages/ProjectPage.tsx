import { Link } from '@tanstack/react-router'
import { projectRoute } from '../router'
import { useProjectQuery } from '../lib/queries'
import { usePalette } from '../lib/palette'
import styles from './ProjectPage.module.css'

export function ProjectPage() {
  const { namespace, project } = projectRoute.useParams()
  const { data, isError } = useProjectQuery(namespace, project)
  const { openPalette } = usePalette()

  if (isError) {
    return (
      <div className={styles.page}>
        <h1>Project not found</h1>
        <p>
          <code>
            {namespace}/{project}
          </code>{' '}
          does not exist in the mock data.
        </p>
        <Link to="/">← Back to repositories</Link>
      </div>
    )
  }

  // Suggest a few top-level docs to open.
  const suggestions = (data?.files ?? [])
    .filter((f) => !f.path.includes('/'))
    .slice(0, 5)

  return (
    <div className={styles.page}>
      <div className={styles.welcome}>
        <div className={styles.kicker}>{namespace}</div>
        <h1 className={styles.title}>{project}</h1>
        <p className={styles.lead}>
          Browse the tree on the left, or jump straight to a file.
        </p>

        <div className={styles.actions}>
          <button className={styles.primaryBtn} onClick={() => openPalette('files')}>
            Go to File <kbd>⌘P</kbd>
          </button>
          <button className={styles.ghostBtn} onClick={() => openPalette('commands')}>
            Command Palette <kbd>⌘K</kbd>
          </button>
        </div>

        {suggestions.length > 0 && (
          <div className={styles.suggest}>
            <div className={styles.suggestHead}>Top-level docs</div>
            <ul className={styles.suggestList}>
              {suggestions.map((f) => (
                <li key={f.path}>
                  <Link
                    to="/p/$namespace/$project/blob/$"
                    params={{ namespace, project, _splat: f.path }}
                    className={styles.suggestLink}
                  >
                    {f.path}
                  </Link>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </div>
  )
}
