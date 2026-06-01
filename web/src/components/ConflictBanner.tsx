import { useState } from 'react'
import styles from './ConflictBanner.module.css'

// The four-button conflict resolution banner (§1.1/§3.5), shown when a SAVE_FILE
// comes back CONFLICT (the file changed under the editor). Each action is the
// user's explicit choice — the buffer is never replaced without one. Save-as and
// Show-diff handlers are optional so the banner can be built up across commits.
export interface ConflictBannerProps {
  message: string
  busy?: boolean
  onDiscardYours: () => void
  onForceOverwrite: () => void
  onSaveAs?: () => void
  onShowDiff?: () => void
}

export function ConflictBanner({
  message,
  busy,
  onDiscardYours,
  onForceOverwrite,
  onSaveAs,
  onShowDiff,
}: ConflictBannerProps) {
  // Force overwrite is destructive, so it takes a second, explicit confirm
  // (inline — no native dialog, consistent with the rest of the UI).
  const [confirmingForce, setConfirmingForce] = useState(false)

  return (
    <div className={styles.banner} role="alert">
      <span className={styles.text}>
        Save failed — this file was modified by someone else.
        {message ? <span className={styles.detail}> {message}</span> : null}
      </span>
      <div className={styles.actions}>
        <button
          className={styles.btn}
          disabled={busy}
          onClick={onDiscardYours}
        >
          Discard mine, load latest
        </button>
        {onSaveAs && (
          <button className={styles.btn} disabled={busy} onClick={onSaveAs}>
            Save as…
          </button>
        )}
        {onShowDiff && (
          <button className={styles.btn} disabled={busy} onClick={onShowDiff}>
            Show diff
          </button>
        )}
        {confirmingForce ? (
          <>
            <button
              className={styles.danger}
              disabled={busy}
              onClick={() => {
                setConfirmingForce(false)
                onForceOverwrite()
              }}
            >
              Confirm overwrite
            </button>
            <button
              className={styles.btn}
              disabled={busy}
              onClick={() => setConfirmingForce(false)}
            >
              Cancel
            </button>
          </>
        ) : (
          <button
            className={styles.dangerOutline}
            disabled={busy}
            onClick={() => setConfirmingForce(true)}
          >
            Force overwrite
          </button>
        )}
      </div>
    </div>
  )
}
