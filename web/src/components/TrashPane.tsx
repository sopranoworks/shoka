import { useEffect, useState } from 'react'
import type { TrashItem } from '../lib/trashQueue'
import styles from './TrashPane.module.css'

// The VS-Code-style collapsible trash pane ("ゴミ箱の中でカウントダウン"): one row
// per reserved file showing its path and a live countdown, with a LARGE,
// mis-click-safe Cancel as the prominent action and a small, separated "Delete
// now" for the impatient. There is no confirm dialog anywhere — the grace IS the
// confirm, so Cancel is deliberately the easy, oversized target.
//
// The pane is opened/collapsed from the activity-rail trash box (which also
// doubles as the drag-to-trash drop target). It is presentational: all state
// lives in the TrashQueue / TrashProvider; this only renders items and a ticking
// countdown derived from each item's deadline.
export function TrashPane({
  items,
  onCancel,
  onDeleteNow,
  onClose,
}: {
  items: TrashItem[]
  onCancel: (id: string) => void
  onDeleteNow: (id: string) => void
  onClose: () => void
}) {
  // Re-render ~4×/s so the countdown ticks down. Mounted only while something is
  // queued (the deadline math needs a clock); idle when empty.
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (items.length === 0) return
    const iv = setInterval(() => setNow(Date.now()), 250)
    return () => clearInterval(iv)
  }, [items.length])

  return (
    <aside
      className={styles.pane}
      role="region"
      aria-label="Trash — files pending deletion"
    >
      <header className={styles.header}>
        <span className={styles.title}>Trash</span>
        <button
          type="button"
          className={styles.close}
          onClick={onClose}
          aria-label="Close trash"
          title="Close"
        >
          ×
        </button>
      </header>

      {items.length === 0 ? (
        <div className={styles.empty}>No files queued for deletion.</div>
      ) : (
        <ul className={styles.list}>
          {items.map((item) => {
            const remainingSec = Math.max(
              0,
              Math.ceil((item.deadline - now) / 1000),
            )
            return (
              <li key={item.id} className={styles.item}>
                <div className={styles.info}>
                  <span className={styles.path} title={item.path}>
                    {item.path}
                  </span>
                  <span className={styles.countdown} aria-live="polite">
                    Deleting in {remainingSec}s
                  </span>
                </div>
                <div className={styles.actions}>
                  {/* LARGE, prominent, mis-click-safe Cancel — the grace's whole
                      point. data-prominent marks the structural intent. */}
                  <button
                    type="button"
                    className={styles.cancel}
                    data-prominent="true"
                    onClick={() => onCancel(item.id)}
                    aria-label={`Cancel deleting ${item.path}`}
                  >
                    Cancel
                  </button>
                  {/* Immediate execute: small and separated, never adjacent-equal
                      to Cancel, so a mis-click lands on Cancel, not destruction. */}
                  <button
                    type="button"
                    className={styles.deleteNow}
                    data-prominent="false"
                    onClick={() => onDeleteNow(item.id)}
                    aria-label={`Delete ${item.path} now`}
                    title="Delete now"
                  >
                    Delete now
                  </button>
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </aside>
  )
}
