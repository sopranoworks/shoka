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
