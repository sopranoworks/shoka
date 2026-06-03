import { useState } from 'react'
import styles from './ConfirmDialog.module.css'

// The target-collision warning (move-file directive §2.8): shown when a move's
// target path is already occupied. The backend refuses a move onto an existing
// target that carries no if_match, returning CONFLICT — so this is NEVER a
// silent overwrite. Exactly three actions, NO diff (a move collision is "two
// different files want one path," not concurrent edits to one file):
//
//   1. Cancel — abort, no change.
//   2. Overwrite — visibly destructive, inline-confirm gated; re-issues the move
//      with if_match = the target's current etag (the explicit-overwrite path).
//   3. Save under a different name — reopen the move dialog at a free path.
export interface MoveCollisionWarningProps {
  targetPath: string
  busy?: boolean
  onCancel: () => void
  onOverwrite: () => void
  onSaveAs: () => void
}

export function MoveCollisionWarning({
  targetPath,
  busy,
  onCancel,
  onOverwrite,
  onSaveAs,
}: MoveCollisionWarningProps) {
  // Overwrite is destructive, so it takes a second explicit confirm (inline — no
  // native dialog), mirroring the editor's Force-overwrite gating.
  const [confirmingOverwrite, setConfirmingOverwrite] = useState(false)

  return (
    <div className={styles.overlay} onClick={onCancel}>
      <div
        className={styles.dialog}
        role="alertdialog"
        aria-modal="true"
        aria-label="A file already exists at that path"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className={styles.title}>A file already exists there</h2>
        <p className={styles.message}>
          <code>{targetPath}</code> already exists. Moving here would replace it.
        </p>
        <div className={styles.actions}>
          <button className={styles.cancel} disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          <button className={styles.cancel} disabled={busy} onClick={onSaveAs}>
            Save under a different name
          </button>
          {confirmingOverwrite ? (
            <button
              className={styles.danger}
              disabled={busy}
              onClick={() => {
                setConfirmingOverwrite(false)
                onOverwrite()
              }}
            >
              Confirm overwrite
            </button>
          ) : (
            <button
              className={styles.danger}
              disabled={busy}
              onClick={() => setConfirmingOverwrite(true)}
            >
              Overwrite
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
