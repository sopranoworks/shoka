import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { recoverProject } from '../lib/fileOps'
import { useToast } from '../lib/toast'
import styles from './RecoverButton.module.css'

// RecoverButton is the in-product recovery control shown on a non-healthy project:
// it re-syncs the project's write-path baseline to the on-disk git HEAD (/ws/ui
// RECOVER_PROJECT → storage.ResyncToHead), clearing a FALSE `corrupted` left by an
// external HEAD move. Non-destructive: a clean-on-disk project returns to healthy;
// a genuinely-drifted one stays corrupted and the toast carries the next step. The
// projects query is invalidated after, so the health badge reflects the outcome.
//
// It lives OUTSIDE the project card's <Link>: onClick stops propagation/prevents
// default so recovering never navigates into the project.
export function RecoverButton({
  namespace,
  project,
}: {
  namespace: string
  project: string
}) {
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const [busy, setBusy] = useState(false)

  async function onRecover(e: React.MouseEvent) {
    e.preventDefault()
    e.stopPropagation()
    if (busy) return
    setBusy(true)
    try {
      const ack = await recoverProject(namespace, project)
      addToast({ level: 'warn', text: ack.message })
    } catch (err) {
      addToast({
        level: 'warn',
        text: `Recover failed: ${
          err instanceof Error ? err.message : 'unknown error'
        }`,
      })
    } finally {
      // Refresh the badge regardless of outcome — the state may have changed.
      queryClient.invalidateQueries({ queryKey: ['projects'] })
      setBusy(false)
    }
  }

  return (
    <button
      type="button"
      className={styles.recoverBtn}
      onClick={onRecover}
      disabled={busy}
      aria-label={`Recover ${namespace}/${project}`}
      title="Re-sync this project to its on-disk git HEAD and clear a false corrupted state"
    >
      {busy ? 'Recovering…' : 'Recover'}
    </button>
  )
}
