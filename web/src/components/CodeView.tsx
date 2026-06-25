import CodeMirror from '@uiw/react-codemirror'
import { EditorView } from '@codemirror/view'
import { languageForPath } from '../lib/cmLanguages'
import { useTheme } from '@shoka/web-core'
import styles from './CodeView.module.css'

// Read-only syntax-highlighted view for non-markdown source files (session 4).
// It reuses the editor's CodeMirror infrastructure (and so its lazy chunk) per
// the directive's "reuse the CodeMirror infrastructure" guidance, rather than a
// second highlighter — markdown fences use rehype-highlight; standalone code
// files use this. Files with no recognised language fall through to a plain
// <pre> in the caller, so this only mounts when there is a language to show.
export function CodeView({ path, content }: { path: string; content: string }) {
  const { theme } = useTheme()
  return (
    <CodeMirror
      value={content}
      readOnly
      editable={false}
      theme={theme === 'dark' ? 'dark' : 'light'}
      extensions={[...languageForPath(path), EditorView.lineWrapping]}
      basicSetup={{ highlightActiveLine: false, foldGutter: false }}
      className={styles.codeView}
    />
  )
}

export default CodeView
