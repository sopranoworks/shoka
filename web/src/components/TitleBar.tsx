import { Link, useRouterState } from '@tanstack/react-router'
import { usePalette } from '../lib/palette'
import styles from './TitleBar.module.css'

// A breadcrumb crumb expressed as typed router link props.
type Crumb =
  | { label: string; kind: 'ns'; ns: string }
  | { label: string; kind: 'project'; ns: string; proj: string }
  | { label: string; kind: 'blob'; ns: string; proj: string; path: string }

// Derive breadcrumb segments (namespace / project / path) from the current
// route, purely from the URL — pathname for the project/file routes and the
// ?ns= search param for the list route — so the trail can never disagree with
// what the page is showing. The header reads as one position trail continuing
// the brand: `Shoka › <namespace> / <project> / <file>`. `Shoka` itself is the
// all-projects home, so there is no standalone label segment; the list root "/"
// yields no crumbs at all.
function useCrumbs(): Crumb[] {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const search = useRouterState({
    select: (s) => s.location.search as { ns?: string },
  })
  const crumbs: Crumb[] = []

  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)(?:\/(?:blob|edit)\/(.*))?$/)
  if (!m) {
    // List route ("/"). Reflect the ?ns= namespace filter as the current
    // position so the trail tracks the filter (`Shoka › <ns>`); bare "/" with
    // no filter yields no segment, leaving the brand standing alone.
    if (typeof search.ns === 'string' && search.ns) {
      crumbs.push({ label: search.ns, kind: 'ns', ns: search.ns })
    }
    return crumbs
  }

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
  // The brand's trailing chevron is the join between `Shoka` (home) and the
  // trail; it is shown ONLY when a breadcrumb segment follows, so the
  // all-projects home reads as a bare `Shoka` with no dangling `›`.
  const hasCrumbs = crumbs.length > 0
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

        <Link
          to="/"
          activeOptions={{ exact: true }}
          className={styles.brand}
          title="All projects"
          aria-label="All projects"
        >
          <span className={styles.brandWord}>Shoka</span>
          {hasCrumbs && (
            <span className={styles.brandChevron} aria-hidden="true">
              ›
            </span>
          )}
        </Link>

        <nav className={styles.crumbs} aria-label="Breadcrumb">
          {crumbs.map((c, i) => {
            // The last segment is the current position: a non-link span marked
            // aria-current="page"; every ancestor is a link.
            const isLast = i === crumbs.length - 1
            return (
              <span key={i} className={styles.crumbItem}>
                {i > 0 && <span className={styles.sep}>/</span>}
                {isLast ? (
                  <span className={styles.crumbCurrent} aria-current="page">
                    {c.label}
                  </span>
                ) : (
                  <CrumbLink crumb={c} />
                )}
              </span>
            )
          })}
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
