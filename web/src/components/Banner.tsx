import { useBanner } from '../lib/banner'
import styles from './Banner.module.css'

// Non-blocking, per-view banner at the top of the content area. Reload triggers
// the explicit re-read (never auto-fires); Dismiss hides it. Navigation also
// clears it (NotifyBridge). One banner at a time — events collapse.
export function Banner() {
  const { banner, clear } = useBanner()
  if (!banner) return null
  return (
    <div className={styles.banner} role="status">
      <span className={styles.text}>
        {banner.text}
        {/* Authorship slot (feasibility §1.2.1) — rendered only if populated;
            no NOTIFY carries an author yet. */}
        {banner.by ? <span className={styles.by}> · by {banner.by}</span> : null}
      </span>
      <div className={styles.actions}>
        <button
          className={styles.reload}
          onClick={() => {
            banner.reload()
            clear()
          }}
        >
          Reload
        </button>
        <button className={styles.dismiss} onClick={clear}>
          Dismiss
        </button>
      </div>
    </div>
  )
}
