import { Link } from '@tanstack/react-router'
import { projectRoute } from '../router'
import { useTreeQuery } from '../lib/queries'
import { flattenFilePaths } from '../lib/tree'
import { usePalette } from '../lib/palette'
import styles from './ProjectPage.module.css'

export function ProjectPage() {
  const { namespace, project } = projectRoute.useParams()
  const { data: tree = [], isError } = useTreeQuery(namespace, project)
  const { openPalette } = usePalette()

  if (isError) {
    return (
      <div className={styles.page}>
        <h1>Project not found</h1>
        <p>
          <code>
            {namespace}/{project}
          </code>{' '}
          could not be loaded.
        </p>
        <Link to="/">← Back to projects</Link>
      </div>
    )
  }

  // Suggest a few top-level docs to open.
  const suggestions = flattenFilePaths(tree)
    .filter((p) => !p.includes('/'))
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
          <Link
            to="/p/$namespace/$project/new"
            params={{ namespace, project }}
            className={styles.ghostBtn}
          >
            New file
          </Link>
          <Link
            to="/p/$namespace/$project/search"
            params={{ namespace, project }}
            className={styles.ghostBtn}
          >
            Search <kbd>⌘⇧F</kbd>
          </Link>
          <button className={styles.ghostBtn} onClick={() => openPalette('commands')}>
            Command Palette <kbd>⌘K</kbd>
          </button>
        </div>

        {suggestions.length > 0 && (
          <div className={styles.suggest}>
            <div className={styles.suggestHead}>Top-level docs</div>
            <ul className={styles.suggestList}>
              {suggestions.map((path) => (
                <li key={path}>
                  <Link
                    to="/p/$namespace/$project/blob/$"
                    params={{ namespace, project, _splat: path }}
                    className={styles.suggestLink}
                  >
                    {path}
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
