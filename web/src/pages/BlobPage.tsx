import { lazy, Suspense } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { blobRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { classifyFile, isHighlightableCode } from '../lib/fileKind'
import { Markdown } from '../components/Markdown'
import styles from './FilePage.module.css'

// CodeView pulls in CodeMirror; lazy-load it so the (markdown-dominant) viewer's
// initial bundle stays free of the editor chunk. It only mounts for non-markdown
// code files, which are rare in the corpus.
const CodeView = lazy(() => import('../components/CodeView'))

export function BlobPage() {
  const { namespace, project, _splat } = blobRoute.useParams()
  const path = _splat ?? ''
  const navigate = useNavigate()
  const { data, isError } = useFileQuery(namespace, project, path)

  // Rendering policy: .md -> rendered markdown (fences highlighted); recognised
  // source files -> read-only highlighted CodeView; other text -> plain <pre>;
  // binary -> placeholder.
  const kind = data ? classifyFile(path, data.content) : null

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
        </span>
        {!isError && (
          <button
            className={styles.editBtn}
            title="Edit this file (⌘E)"
            onClick={() =>
              void navigate({
                to: '/p/$namespace/$project/edit/$',
                params: { namespace, project, _splat: path },
              })
            }
          >
            <EditGlyph />
            Edit
          </button>
        )}
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

function EditGlyph() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
    >
      <path
        d="M11.5 2.5l2 2L6 12l-2.5.5L4 10z"
        stroke="currentColor"
        strokeWidth="1.2"
        strokeLinejoin="round"
      />
    </svg>
  )
}
