import { wsClient } from './wsClient'
import type { FileContent, SaveAck, ConflictPayload } from './types'

// Imperative /ws/ui file operations for the editor. Unlike the read queries
// (lib/queries), these are user-initiated mutations, so they live outside
// TanStack Query and surface the SAVE_FILE response type explicitly: a save can
// come back as SAVE_ACK (ok) or CONFLICT (stale if_match), and the two must not
// be confused (the wsClient now exposes the frame type via requestFrame).

export type SaveResult =
  | { ok: true; path: string; etag: string }
  | { ok: false; path: string; currentEtag: string; message: string }

export interface SaveArgs {
  namespace: string
  project: string
  path: string
  content: string
  // The optimistic-concurrency etag. null/undefined => unchecked write, which
  // creates the file if the path does not exist (precursor §7 storage
  // precondition) — used for "Save as" to a new path and "Save mine as new
  // file" after an external delete.
  ifMatch?: string | null
}

export async function saveFile(args: SaveArgs): Promise<SaveResult> {
  const payload: Record<string, unknown> = {
    namespace: args.namespace,
    projectName: args.project,
    path: args.path,
    content: args.content,
  }
  if (args.ifMatch != null) payload.if_match = args.ifMatch

  const frame = await wsClient().requestFrame('SAVE_FILE', payload)
  if (frame.type === 'CONFLICT') {
    const p = frame.payload as ConflictPayload
    return {
      ok: false,
      path: p.path,
      currentEtag: p.current_etag,
      message: p.message,
    }
  }
  const p = frame.payload as SaveAck
  return { ok: true, path: p.path, etag: p.etag }
}

// A fresh read straight off /ws/ui (not the query cache), for "Discard mine,
// load latest" and "Show diff" which need the current server content + etag.
export function readFileFresh(
  namespace: string,
  project: string,
  path: string,
): Promise<FileContent> {
  return wsClient().request<FileContent>('READ_FILE', {
    namespace,
    projectName: project,
    path,
  })
}

// Whether a path already exists, and its current etag if so. READ_FILE rejects
// (ERROR) for a missing file, which is the "does not exist" signal used by the
// "Save as" overwrite-confirm flow.
export async function fileExists(
  namespace: string,
  project: string,
  path: string,
): Promise<{ exists: boolean; etag?: string }> {
  try {
    const f = await readFileFresh(namespace, project, path)
    return { exists: true, etag: f.etag }
  } catch {
    return { exists: false }
  }
}
