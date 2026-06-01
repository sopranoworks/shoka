import { useEffect, useMemo, useState } from 'react'
import CodeMirror from '@uiw/react-codemirror'
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels'
import { editRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { useEditorBuffer } from '../lib/useEditorBuffer'
import { useDebouncedValue } from '../lib/useDebouncedValue'
import { useMediaQuery } from '../lib/useMediaQuery'
import { useTheme } from '../lib/theme'
import { classifyFile } from '../lib/fileKind'
import { Markdown } from '../components/Markdown'
import styles from './EditorPage.module.css'

const PREVIEW_DEBOUNCE_MS = 200

// Editor for an existing file (session 3). Loads the file over /ws/ui READ_FILE
// (which returns {path, content, etag}), copies the content into an in-memory
// buffer, and keeps the etag for the eventual save's if_match. The buffer is
// decoupled from the TanStack Query cache, so a background invalidation never
// silently replaces what the user is editing (§2).
//
// Markdown files get a two-pane source+preview split (preview closeable,
// resizable, debounced); everything else gets a single full-width source pane
// (no preview — non-markdown has nothing to render).
export function EditorPage() {
  const { namespace, project, _splat } = editRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)
  const { theme } = useTheme()
  const buf = useEditorBuffer()
  const { initialized, load } = buf
  const isNarrow = useMediaQuery('(max-width: 640px)')

  const isMarkdown = useMemo(
    () => classifyFile(path, buf.baseline) === 'markdown',
    [path, buf.baseline],
  )
  const [previewOpen, setPreviewOpen] = useState(true)

  // Preview re-renders 200ms after the last keystroke (below the perceptible
  // threshold), so a burst of typing does not thrash react-markdown.
  const previewSource = useDebouncedValue(buf.content, PREVIEW_DEBOUNCE_MS)

  // Initialize the buffer once, when the file first loads. Subsequent cache
  // changes do not touch the buffer (no silent overwrite).
  useEffect(() => {
    if (data && !initialized) load(data.content, data.etag)
  }, [data, initialized, load])

  const cm = (
    <CodeMirror
      value={buf.content}
      onChange={buf.setContent}
      theme={theme === 'dark' ? 'dark' : 'light'}
      height="100%"
      className={styles.editor}
    />
  )

  const showSplit = isMarkdown && previewOpen

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
        </span>
        <div className={styles.toolbarActions}>
          {isMarkdown && (
            <button
              className={styles.ghostBtn}
              onClick={() => setPreviewOpen((v) => !v)}
            >
              {previewOpen ? 'Hide preview' : 'Show preview'}
            </button>
          )}
        </div>
      </div>

      <div className={styles.body}>
        {isError ? (
          <div className={styles.error}>
            File not found: <code>{path}</code>
          </div>
        ) : !initialized ? (
          <div className={styles.loading}>Loading…</div>
        ) : showSplit ? (
          <PanelGroup
            key={isNarrow ? 'v' : 'h'}
            direction={isNarrow ? 'vertical' : 'horizontal'}
            autoSaveId={isNarrow ? undefined : 'shoka-editor-split'}
            className={styles.split}
          >
            <Panel id="source" order={1} minSize={20} className={styles.sourcePanel}>
              {cm}
            </Panel>
            <PanelResizeHandle
              className={isNarrow ? styles.resizeHandleV : styles.resizeHandle}
            />
            <Panel
              id="preview"
              order={2}
              minSize={20}
              className={styles.previewPanel}
            >
              <div className={styles.previewHead}>
                <span>Preview</span>
                <button
                  className={styles.closePreview}
                  title="Close preview"
                  aria-label="Close preview"
                  onClick={() => setPreviewOpen(false)}
                >
                  ×
                </button>
              </div>
              <div className={styles.previewBody}>
                <Markdown content={previewSource} />
              </div>
            </Panel>
          </PanelGroup>
        ) : (
          <div className={styles.singlePane}>{cm}</div>
        )}
      </div>
    </div>
  )
}
