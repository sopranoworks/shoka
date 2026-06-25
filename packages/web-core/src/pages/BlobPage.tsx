import { lazy, Suspense, useEffect, useMemo, useRef } from 'react'
import { useParams, useElementScrollRestoration } from '@tanstack/react-router'
import { useFileQuery, useTreeQuery } from '../lib/queries'
import { classifyFile, isHighlightableCode } from '../lib/fileKind'
import { Markdown } from '../components/Markdown'
import { useContentConfig } from '../lib/contentConfig'
import type { FileNode } from '../lib/types'
import styles from './FilePage.module.css'

const CodeView = lazy(() => import('../components/CodeView'))

function findModifiedAt(nodes: FileNode[], target: string): string | undefined {
  for (const n of nodes) {
    if (!n.isDir && n.path === target) return n.modifiedAt
    if (n.isDir && n.children) {
      const found = findModifiedAt(n.children, target)
      if (found) return found
    }
  }
}

function formatDate(iso: string): string {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

export function BlobPage() {
  const { namespace, project, _splat } = useParams({ strict: false }) as {
    namespace: string
    project: string
    _splat?: string
  }
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)
  const { data: tree } = useTreeQuery(namespace, project)
  const { renderEditButton } = useContentConfig()
  const modifiedAt = useMemo(
    () => (tree && path ? findModifiedAt(tree, path) : undefined),
    [tree, path],
  )

  const scrollEntry = useElementScrollRestoration({ id: 'file-body' })
  const bodyRef = useRef<HTMLDivElement>(null)
  const restoredRef = useRef(false)
  useEffect(() => {
    if (restoredRef.current) return
    if (data && scrollEntry?.scrollY && bodyRef.current) {
      bodyRef.current.scrollTop = scrollEntry.scrollY
      restoredRef.current = true
    }
  }, [data, scrollEntry])

  const kind = data ? classifyFile(path, data.content) : null

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
        </span>
        {modifiedAt && (
          <span className={styles.fileDate} title={modifiedAt}>
            {formatDate(modifiedAt)}
          </span>
        )}
        {!isError && renderEditButton && renderEditButton(namespace, project, path, styles)}
      </div>

      <div
        ref={bodyRef}
        className={styles.body}
        data-scroll-restoration-id="file-body"
      >
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
        ) : isHighlightableCode(path) ? (
          <Suspense fallback={<pre className={styles.plain}>{data.content}</pre>}>
            <CodeView path={path} content={data.content} />
          </Suspense>
        ) : (
          <pre className={styles.plain}>{data.content}</pre>
        )}
      </div>
    </div>
  )
}
