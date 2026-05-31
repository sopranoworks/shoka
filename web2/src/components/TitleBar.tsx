import { Link, useRouterState } from '@tanstack/react-router'
import { usePalette } from '../lib/palette'
import styles from './TitleBar.module.css'

// A breadcrumb crumb expressed as typed router link props.
type Crumb =
  | { label: string; kind: 'ns'; ns: string }
  | { label: string; kind: 'project'; ns: string; proj: string }
  | { label: string; kind: 'blob'; ns: string; proj: string; path: string }

// Derive breadcrumb segments (namespace / project / path) from the current URL.
function useCrumbs(): Crumb[] {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const crumbs: Crumb[] = []

  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)(?:\/(?:blob|edit)\/(.*))?$/)
  if (!m) return crumbs

  const ns = decodeURIComponent(m[1])
  const proj = decodeURIComponent(m[2])
  const rest = m[3]

  crumbs.push({ label: ns, kind: 'ns', ns })
  crumbs.push({ label: proj, kind: 'project', ns, proj })

  if (rest) {
    const segs = rest.split('/').filter(Boolean)
    let accum = ''
    segs.forEach((seg) => {
      accum = accum ? `${accum}/${seg}` : seg
      crumbs.push({ label: seg, kind: 'blob', ns, proj, path: accum })
    })
  }
  return crumbs
}

function CrumbLink({ crumb }: { crumb: Crumb }) {
  const cls = styles.crumbLink
  switch (crumb.kind) {
    case 'ns':
      return (
        <Link to="/" search={{ ns: crumb.ns }} className={cls}>
          {crumb.label}
        </Link>
      )
    case 'project':
      return (
        <Link
          to="/p/$namespace/$project"
          params={{ namespace: crumb.ns, project: crumb.proj }}
          className={cls}
        >
          {crumb.label}
        </Link>
      )
    case 'blob':
      return (
        <Link
          to="/p/$namespace/$project/blob/$"
          params={{
            namespace: crumb.ns,
            project: crumb.proj,
            _splat: crumb.path,
          }}
          className={cls}
        >
          {crumb.label}
        </Link>
      )
  }
}

export function TitleBar({
  onToggleSidebar,
}: {
  onToggleSidebar: () => void
}) {
  const crumbs = useCrumbs()
  const { openPalette } = usePalette()

  return (
    <header className={styles.bar}>
      <div className={styles.left}>
        <button
          className={styles.iconBtn}
          onClick={onToggleSidebar}
          title="Toggle sidebar"
          aria-label="Toggle sidebar"
        >
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <rect
              x="1.5"
              y="2.5"
              width="13"
              height="11"
              rx="1.5"
              stroke="currentColor"
            />
            <line x1="6" y1="2.5" x2="6" y2="13.5" stroke="currentColor" />
          </svg>
        </button>

        <Link to="/" className={styles.brand}>
          蕉<span className={styles.brandWord}>shoka</span>
        </Link>

        <nav className={styles.crumbs} aria-label="Breadcrumb">
          {crumbs.length === 0 ? (
            <span className={styles.crumbDim}>repositories</span>
          ) : (
            crumbs.map((c, i) => (
              <span key={i} className={styles.crumbItem}>
                {i > 0 && <span className={styles.sep}>/</span>}
                <CrumbLink crumb={c} />
              </span>
            ))
          )}
        </nav>
      </div>

      <button
        className={styles.commandCentre}
        onClick={() => openPalette('commands')}
        title="Command palette"
      >
        <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
          <path
            d="M2 4l4 4-4 4M8 12h6"
            stroke="currentColor"
            strokeWidth="1.4"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
        <span className={styles.ccLabel}>Search or run a command</span>
        <kbd className={styles.ccKbd}>⌘K</kbd>
      </button>

      <div className={styles.right} />
    </header>
  )
}
