import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import CodeMirror from '@uiw/react-codemirror'
import { markdown } from '@codemirror/lang-markdown'
import { oneDark } from '@codemirror/theme-one-dark'
import { editRoute } from '../router'
import { useFileQuery } from '../lib/queries'
import { useTheme } from '../lib/theme'
import styles from './FilePage.module.css'

export function EditPage() {
  const { namespace, project, _splat } = editRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)
  const { theme } = useTheme()

  const [value, setValue] = useState('')
  const [dirty, setDirty] = useState(false)

  // Seed the editor with the same mock content the viewer shows.
  useEffect(() => {
    if (data) {
      setValue(data.content)
      setDirty(false)
    }
  }, [data])

  const onSave = () => {
    // Stub: real persistence (Shoka write_file) is out of scope.
    // eslint-disable-next-line no-alert
    alert('mock save')
    setDirty(false)
  }

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
          {dirty && <span className={styles.dirty}> ●</span>}
        </span>
        <div className={styles.toolbarRight}>
          <Link
            to="/p/$namespace/$project/blob/$"
            params={{ namespace, project, _splat: path }}
            className={styles.previewLink}
          >
            Preview
          </Link>
          <button className={styles.saveBtn} onClick={onSave}>
            Save
          </button>
        </div>
      </div>

      <div className={styles.editorBody}>
        {isError ? (
          <div className={styles.error} style={{ padding: 24 }}>
            File not found: <code>{path}</code>
          </div>
        ) : (
          <CodeMirror
            value={value}
            height="100%"
            style={{ height: '100%', fontSize: 14 }}
            theme={theme === 'dark' ? oneDark : 'light'}
            extensions={[markdown()]}
            onChange={(v) => {
              setValue(v)
              setDirty(true)
            }}
            basicSetup={{
              lineNumbers: true,
              highlightActiveLine: true,
              foldGutter: true,
            }}
          />
        )}
      </div>
    </div>
  )
}
