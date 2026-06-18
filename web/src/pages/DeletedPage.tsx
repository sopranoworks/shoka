import { useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useIsAdmin } from '../lib/admin'
import { listDeleted, reviveFile, type DeletedEntry } from '../lib/deletedOps'
import { useToast } from '../lib/toast'
import styles from './DeletedPage.module.css'

// The administrator-only "Deleted files" view (B-28, the 2026-06-18 deleted-log
// directive). It lists a project's currently-deleted files from the cheap
// deleted-file log (no git walk) and lets an admin REVIVE one — re-creating its
// last content as a NEW commit (forward-only; history preserved). It also offers a
// name-specified restore (type a path) for the last-resort case when a deletion
// has aged out of the log. The server gate is authoritative; useIsAdmin() only
// governs UI exposure. This is DISTINCT from the client-side grace-period trash
// can: that is recent + undoable; this is the full git past with no grace — so the
// revive is a simple confirm-free action (it is not destructive, only additive).
export function DeletedPage() {
  const isAdmin = useIsAdmin()
  const params = useParams({ strict: false }) as {
    namespace?: string
    project?: string
  }
  const namespace = params.namespace ?? ''
  const project = params.project ?? ''
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [namePath, setNamePath] = useState('')

  const {
    data: deleted,
    isLoading,
    isError,
    error,
    refetch,
    isFetching,
  } = useQuery({
    queryKey: ['deleted', namespace, project],
    enabled: isAdmin && namespace !== '' && project !== '',
    queryFn: () => listDeleted(namespace, project),
  })

  const revive = useMutation({
    mutationFn: (path: string) => reviveFile(namespace, project, path),
    onSuccess: (_data, path) => {
      addToast({ level: 'warn', text: `Revived ${path}` })
      // The file reappears in the tree; refresh the tree and the deleted list.
      void queryClient.invalidateQueries({ queryKey: ['tree', namespace, project] })
      void queryClient.invalidateQueries({ queryKey: ['deleted', namespace, project] })
      setNamePath('')
    },
    onError: (e: unknown) => {
      const message = e instanceof Error ? e.message : 'revive failed'
      addToast({ level: 'warn', text: message })
    },
  })

  if (!isAdmin) {
    return (
      <div className={styles.page} data-testid="deleted-page">
        <p className={styles.empty}>You do not have permission to view deleted files.</p>
      </div>
    )
  }

  return (
    <div className={styles.page} data-testid="deleted-page">
      <header className={styles.header}>
        <h1 className={styles.title}>Deleted files</h1>
        <span className={styles.subtitle}>
          {namespace}/{project}
        </span>
        <button
          type="button"
          className={styles.refresh}
          onClick={() => void refetch()}
          disabled={isFetching}
        >
          Refresh
        </button>
      </header>

      <form
        className={styles.nameRestore}
        onSubmit={(e) => {
          e.preventDefault()
          const p = namePath.trim()
          if (p) revive.mutate(p)
        }}
      >
        <input
          type="text"
          className={styles.nameInput}
          placeholder="Restore by path (e.g. docs/old.md)…"
          aria-label="Restore by path"
          value={namePath}
          onChange={(e) => setNamePath(e.target.value)}
        />
        <button
          type="submit"
          className={styles.reviveBtn}
          disabled={revive.isPending || namePath.trim() === ''}
        >
          Restore
        </button>
      </form>

      {isLoading && <p className={styles.empty}>Loading…</p>}
      {isError && (
        <p className={styles.error}>
          {error instanceof Error ? error.message : 'failed to load deleted files'}
        </p>
      )}
      {!isLoading && !isError && (deleted?.length ?? 0) === 0 && (
        <p className={styles.empty} data-testid="deleted-empty">
          No deleted files.
        </p>
      )}

      {(deleted?.length ?? 0) > 0 && (
        <ul className={styles.list} data-testid="deleted-list">
          {deleted!.map((d: DeletedEntry) => (
            <li key={d.path} className={styles.row} data-testid={`deleted-row-${d.path}`}>
              <span className={styles.path}>{d.path}</span>
              <span className={styles.commit} title={d.deletionCommit}>
                {d.deletionCommit.slice(0, 8)}
              </span>
              <button
                type="button"
                className={styles.reviveBtn}
                onClick={() => revive.mutate(d.path)}
                disabled={revive.isPending}
                data-testid={`revive-${d.path}`}
              >
                Revive
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
