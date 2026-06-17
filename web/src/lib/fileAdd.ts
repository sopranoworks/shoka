import { saveFile } from './fileOps'

// External file drag-and-drop ADD (B-28). The browser-side half of the drop:
// validate the file format, read its bytes byte-faithfully (base64), and write it
// over the existing /ws/ui SAVE_FILE path with content_encoding:"base64" — the
// SAME funnel + allowlist + authz the MCP write_file tool uses. A name collision
// is REFUSED by the server (CONFLICT, no silent overwrite); the caller confirms,
// and we resend with the current etag as if_match.

// The closed allowlist, mirroring the server's internal/ingest set. The client
// check is for a clear, immediate rejection (do not send a doomed write); the
// server enforces it authoritatively regardless.
export const ALLOWED_ADD_EXTS = ['.md', '.markdown', '.json', '.yaml', '.yml']
export const ALLOWED_ADD_FORMATS = ALLOWED_ADD_EXTS.join(', ')

// extOf extracts the lowercase extension the way Go's filepath.Ext does: from the
// last dot of the basename. An extensionless name (Makefile, LICENSE) returns ''.
export function extOf(name: string): string {
  const base = name.slice(name.lastIndexOf('/') + 1)
  const dot = base.lastIndexOf('.')
  return dot < 0 ? '' : base.slice(dot).toLowerCase()
}

export function isAllowedAddFile(name: string): boolean {
  return ALLOWED_ADD_EXTS.includes(extOf(name))
}

// joinAddPath resolves a dropped file's destination: its name under destDir, or
// at the project root when destDir is empty.
export function joinAddPath(destDir: string, name: string): string {
  const dir = destDir.replace(/\/+$/, '')
  return dir ? `${dir}/${name}` : name
}

// fileToBase64 reads a File's raw bytes as base64 (no data: prefix), so genuinely
// non-UTF-8 content survives the JSON wire intact (the B-46a lesson).
export function fileToBase64(file: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => {
      const res = String(reader.result)
      const comma = res.indexOf(',')
      resolve(comma >= 0 ? res.slice(comma + 1) : res)
    }
    reader.onerror = () => reject(reader.error ?? new Error('file read failed'))
    reader.readAsDataURL(file)
  })
}

export type AddStatus =
  | 'added' // created at a fresh path
  | 'overwritten' // existed, user confirmed, replaced
  | 'rejected' // format outside the allowlist (not sent)
  | 'skipped' // existed, user declined to overwrite
  | 'error' // read/write failure

export interface AddOutcome {
  name: string
  path: string
  status: AddStatus
  message?: string
}

// addDroppedFile runs one dropped file through the add flow. confirmOverwrite is
// asked ONLY when the server refuses a collision (CONFLICT); returning false keeps
// the existing file untouched. It never throws — every failure is an outcome.
export async function addDroppedFile(args: {
  namespace: string
  project: string
  destDir: string
  file: File
  confirmOverwrite: (path: string) => boolean | Promise<boolean>
}): Promise<AddOutcome> {
  const { namespace, project, destDir, file, confirmOverwrite } = args
  const path = joinAddPath(destDir, file.name)

  if (!isAllowedAddFile(file.name)) {
    return {
      name: file.name,
      path,
      status: 'rejected',
      message: `${file.name} was not added — only ${ALLOWED_ADD_FORMATS} files can be added`,
    }
  }

  let content: string
  try {
    content = await fileToBase64(file)
  } catch {
    return { name: file.name, path, status: 'error', message: `Could not read ${file.name}` }
  }

  try {
    const first = await saveFile({
      namespace,
      project,
      path,
      content,
      contentEncoding: 'base64',
    })
    if (first.ok) return { name: file.name, path, status: 'added' }

    // Server refused a collision (no silent overwrite). Confirm, then resend with
    // the current etag as if_match to overwrite intentionally.
    const proceed = await confirmOverwrite(path)
    if (!proceed) {
      return { name: file.name, path, status: 'skipped', message: `Kept the existing ${path}` }
    }
    const second = await saveFile({
      namespace,
      project,
      path,
      content,
      contentEncoding: 'base64',
      ifMatch: first.currentEtag,
    })
    if (second.ok) return { name: file.name, path, status: 'overwritten' }
    return { name: file.name, path, status: 'error', message: second.message }
  } catch (e) {
    return {
      name: file.name,
      path,
      status: 'error',
      message: e instanceof Error ? e.message : `Could not add ${file.name}`,
    }
  }
}
