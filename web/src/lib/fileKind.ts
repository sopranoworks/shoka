// Classify a file for the viewer (directive §1.4.1): markdown is rendered,
// other text is shown as plain <pre>, binary gets a placeholder. Syntax
// highlighting for source files is deferred to session 4.

export type FileKind = 'markdown' | 'text' | 'binary'

const BINARY_EXTS = new Set([
  'png', 'jpg', 'jpeg', 'gif', 'webp', 'ico', 'bmp', 'svg', 'pdf', 'zip', 'gz',
  'tar', 'tgz', 'wasm', 'woff', 'woff2', 'ttf', 'otf', 'eot', 'mp3', 'mp4',
  'mov', 'avi', 'webm', 'exe', 'dll', 'so', 'dylib', 'bin', 'class', 'o',
])

// A NUL byte indicates non-text content (the read returned raw bytes).
const NUL = String.fromCharCode(0)

export function classifyFile(path: string, content: string): FileKind {
  const ext = path.includes('.') ? path.split('.').pop()!.toLowerCase() : ''
  if (BINARY_EXTS.has(ext)) return 'binary'
  if (content.indexOf(NUL) !== -1) return 'binary'
  if (ext === 'md' || ext === 'markdown') return 'markdown'
  return 'text'
}

// Extensions the read-only CodeView can syntax-highlight, kept in sync with
// lib/cmLanguages' languageForPath. This is a plain extension check (no
// CodeMirror imports) so the viewer can decide whether to lazy-load CodeView
// without pulling the editor chunk into the initial bundle.
const CODE_EXTS = new Set([
  'json', 'yaml', 'yml', 'js', 'jsx', 'mjs', 'cjs', 'ts', 'tsx', 'go',
])

// isHighlightableCode reports whether a non-markdown text file has a language
// the CodeView highlights. .txt and unknown extensions return false and stay a
// plain <pre> — correct for the corpus, and avoids loading CodeMirror to show
// plain text.
export function isHighlightableCode(path: string): boolean {
  const ext = path.includes('.') ? path.split('.').pop()!.toLowerCase() : ''
  return CODE_EXTS.has(ext)
}
