import { wsClient } from './wsClient'
import type { FileContent, SaveAck, ConflictPayload, MoveAck, DeleteAck } from './types'

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

// The single imperative move op all move surfaces (dialog, inline rename,
// drag-drop, context menu, palette) funnel through — no surface talks to wsClient
// directly. A move is a PURE PATH CHANGE (B-33): the backend renames atomically
// and rewrites NO links, so MOVE_ACK's links_rewritten is always 0. We read it
// off the frame but deliberately do NOT surface or branch on it — there is no
// link concern in this UI (move-file directive §0).
export type MoveResult =
  | { ok: true; sourcePath: string; targetPath: string; newEtag: string }
  | { ok: false; path: string; currentEtag: string; message: string }

export interface MoveArgs {
  namespace: string
  project: string
  sourcePath: string
  targetPath: string
  // Optimistic-concurrency etag, with the backend's dual semantic: when the
  // target exists it guards the target (explicit overwrite); when the target is
  // free it guards the source. The default move sends none — so a move onto an
  // occupied target is REFUSED (CONFLICT), never a silent overwrite (§2.8).
  ifMatch?: string | null
}

export async function moveFile(args: MoveArgs): Promise<MoveResult> {
  const payload: Record<string, unknown> = {
    namespace: args.namespace,
    projectName: args.project,
    source_path: args.sourcePath,
    target_path: args.targetPath,
  }
  if (args.ifMatch != null) payload.if_match = args.ifMatch

  const frame = await wsClient().requestFrame('MOVE_FILE', payload)
  if (frame.type === 'CONFLICT') {
    const p = frame.payload as ConflictPayload
    return {
      ok: false,
      path: p.path,
      currentEtag: p.current_etag,
      message: p.message,
    }
  }
  const p = frame.payload as MoveAck
  // p.links_rewritten is present and always 0 — intentionally ignored.
  return {
    ok: true,
    sourcePath: p.source_path,
    targetPath: p.target_path,
    newEtag: p.new_etag,
  }
}

// The single imperative delete op the trash queue funnels through (B-31). A
// delete is DEFERRED behind a client-side grace timer: the trash queue calls this
// ONLY when the timer elapses, carrying the if_match etag captured at enqueue, so
// a cancelled reservation never reaches the wire (nothing to undo) and a file
// edited mid-grace comes back as a CONFLICT (not a silent destroy). It wires the
// existing /ws/ui DELETE_FILE over storage.Delete — a git-tracked hard-remove,
// recoverable via History.
export type DeleteResult =
  | { ok: true; path: string }
  | { ok: false; path: string; currentEtag: string; message: string }

export interface DeleteArgs {
  namespace: string
  project: string
  path: string
  // The optimistic-concurrency etag captured when the file was enqueued into the
  // trash. A stale etag (the file changed during the grace) → CONFLICT. Omitted
  // only when no etag was captured (unchecked delete).
  ifMatch?: string | null
}

export async function deleteFile(args: DeleteArgs): Promise<DeleteResult> {
  const payload: Record<string, unknown> = {
    namespace: args.namespace,
    projectName: args.project,
    path: args.path,
  }
  if (args.ifMatch != null) payload.if_match = args.ifMatch

  const frame = await wsClient().requestFrame('DELETE_FILE', payload)
  if (frame.type === 'CONFLICT') {
    const p = frame.payload as ConflictPayload
    return {
      ok: false,
      path: p.path,
      currentEtag: p.current_etag,
      message: p.message,
    }
  }
  const p = frame.payload as DeleteAck
  return { ok: true, path: p.path }
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
