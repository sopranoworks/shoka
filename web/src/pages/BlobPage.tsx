import { blobRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { Markdown } from '../components/Markdown'
import styles from './FilePage.module.css'

export function BlobPage() {
  const { namespace, project, _splat } = blobRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
        </span>
      </div>

      <div className={styles.body}>
        {isError ? (
          <div className={styles.error}>
            File not found: <code>{path}</code>
          </div>
        ) : data ? (
          <Markdown content={data.content} />
        ) : (
          <div className={styles.loading}>Loading…</div>
        )}
      </div>
    </div>
  )
}
