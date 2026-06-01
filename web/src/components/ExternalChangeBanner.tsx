import styles from './ExternalChangeBanner.module.css'

// The edit route's external-change banner (§1.1 / §3.6). Raised when a NOTIFY
// reports the file under edit changed (write) or was deleted by someone else.
// It NEVER touches the editor buffer — every action is the user's explicit
// choice. For a write, "Resolve now" surfaces the same conflict-resolution UX a
// save conflict would; for a delete, the user can re-save their buffer as a
// (re)created file or abandon it.
export interface ExternalChangeBannerProps {
  kind: 'write' | 'delete'
  busy?: boolean
  onResolve: () => void // write: open conflict resolution proactively
  onSaveAsNew: () => void // delete: save my buffer as a new/recreated file
  onDiscardToTree: () => void // delete: discard buffer, go to the project tree
  onDismiss: () => void
}

export function ExternalChangeBanner({
  kind,
  busy,
  onResolve,
  onSaveAsNew,
  onDiscardToTree,
  onDismiss,
}: ExternalChangeBannerProps) {
  return (
    <div className={styles.banner} role="status">
      <span className={styles.text}>
        {kind === 'delete'
          ? 'This file was deleted by someone else.'
          : 'This file was modified by someone else.'}
      </span>
      <div className={styles.actions}>
        {kind === 'write' ? (
          <button className={styles.btn} disabled={busy} onClick={onResolve}>
            Resolve now
          </button>
        ) : (
          <>
            <button className={styles.btn} disabled={busy} onClick={onSaveAsNew}>
              Save mine as new file
            </button>
            <button
              className={styles.btn}
              disabled={busy}
              onClick={onDiscardToTree}
            >
              Discard mine, go to tree
            </button>
          </>
        )}
        <button className={styles.dismiss} disabled={busy} onClick={onDismiss}>
          Dismiss
        </button>
      </div>
    </div>
  )
}
