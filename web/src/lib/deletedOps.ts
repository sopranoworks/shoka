import { wsClient } from '@shoka/web-core'

// Client ops for the admin deleted-file log (the 2026-06-18 deleted-log
// directive). list_deleted is a cheap read of the per-project deleted-file log;
// revive_file re-creates a deleted file forward-only. Both are admin-only — the
// server is the authoritative gate (manager.go wsLevels); the admin predicate only
// decides whether the surface is EXPOSED.

export interface DeletedEntry {
  path: string
  deletionCommit: string
  deletedAt: string
}

interface ListDeletedAck {
  namespace: string
  projectName: string
  deleted: DeletedEntry[]
}

interface ReviveAck {
  namespace: string
  projectName: string
  path: string
  revived: boolean
}

// listDeleted fetches a project's currently-deleted files (a cheap log read).
export async function listDeleted(
  namespace: string,
  project: string,
): Promise<DeletedEntry[]> {
  const ack = await wsClient().request<ListDeletedAck>('LIST_DELETED', {
    namespace,
    projectName: project,
  })
  return ack.deleted ?? []
}

// reviveFile re-creates a deleted file forward-only. fromCommit optionally
// overrides the recorded deletion commit (the name-specified last resort path is
// also reachable by passing a path not in the log). Rejects (ERROR frame) with the
// divergence message when git no longer has the deletion.
export async function reviveFile(
  namespace: string,
  project: string,
  path: string,
  fromCommit?: string,
): Promise<ReviveAck> {
  const payload: Record<string, unknown> = {
    namespace,
    projectName: project,
    path,
  }
  if (fromCommit) payload.fromCommit = fromCommit
  return wsClient().request<ReviveAck>('REVIVE_FILE', payload)
}
