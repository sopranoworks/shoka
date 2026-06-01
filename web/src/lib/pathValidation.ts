// Minimal sanity validation for a user-entered file path in the path-less
// new-file workflow (session 4 §3.7). Returns an error message to show inline,
// or null when the path is acceptable. Deliberately permissive about characters
// (the storage layer accepts arbitrary relative paths); it only blocks the ways
// a path would be malformed or escape the project.
export function validateFilePath(path: string): string | null {
  const p = path.trim()
  if (p === '') return 'Enter a file path.'
  if (p.startsWith('/')) return 'Path must be relative (no leading slash).'
  if (p.endsWith('/')) return 'Path must be a file, not a directory.'
  const segments = p.split('/')
  if (segments.some((s) => s === '')) return 'Path has an empty segment ("//").'
  if (segments.some((s) => s === '.' || s === '..'))
    return 'Path cannot contain "." or ".." segments.'
  return null
}
