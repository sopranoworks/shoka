import { blobRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { classifyFile } from '../lib/fileKind'
import { Markdown } from '../components/Markdown'
import styles from './FilePage.module.css'

export function BlobPage() {
  const { namespace, project, _splat } = blobRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)

  // Rendering policy (§1.4.1): .md -> rendered markdown; other text -> plain
  // <pre>; binary -> placeholder. Syntax highlighting is session 4.
  const kind = data ? classifyFile(path, data.content) : null

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
        ) : !data ? (
          <div className={styles.loading}>Loading…</div>
        ) : kind === 'markdown' ? (
          <Markdown content={data.content} />
        ) : kind === 'binary' ? (
          <div className={styles.placeholder}>
            Binary file — cannot preview.
          </div>
        ) : (
          <pre className={styles.plain}>{data.content}</pre>
        )}
      </div>
    </div>
  )
}
