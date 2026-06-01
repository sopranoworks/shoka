import { useEffect, useState } from 'react'
import styles from './RouteFallback.module.css'

// Suspense fallback for lazily code-split routes. It waits ~120ms before showing
// anything so a fast chunk fetch doesn't flash a loading state (directive §3.5).
export function RouteFallback() {
  const [show, setShow] = useState(false)
  useEffect(() => {
    const t = setTimeout(() => setShow(true), 120)
    return () => clearTimeout(t)
  }, [])
  return show ? <div className={styles.fallback}>Loading…</div> : null
}
