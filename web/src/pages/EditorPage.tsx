import { useEffect } from 'react'
import CodeMirror from '@uiw/react-codemirror'
import { editRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { useEditorBuffer } from '../lib/useEditorBuffer'
import { useTheme } from '../lib/theme'
import styles from './EditorPage.module.css'

// Editor for an existing file (session 3). Loads the file over /ws/ui READ_FILE
// (which returns {path, content, etag}), copies the content into an in-memory
// buffer, and keeps the etag for the eventual save's if_match. The buffer is
// decoupled from the TanStack Query cache, so a background invalidation never
// silently replaces what the user is editing (§2).
export function EditorPage() {
  const { namespace, project, _splat } = editRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)
  const { theme } = useTheme()
  const buf = useEditorBuffer()
  const { initialized, load } = buf

  // Initialize the buffer once, when the file first loads. Subsequent cache
  // changes do not touch the buffer (no silent overwrite).
  useEffect(() => {
    if (data && !initialized) load(data.content, data.etag)
  }, [data, initialized, load])

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
        ) : !initialized ? (
          <div className={styles.loading}>Loading…</div>
        ) : (
          <CodeMirror
            value={buf.content}
            onChange={buf.setContent}
            theme={theme === 'dark' ? 'dark' : 'light'}
            height="100%"
            className={styles.editor}
          />
        )}
      </div>
    </div>
  )
}
