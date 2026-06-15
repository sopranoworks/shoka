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

  const m = pathname.match(
    /^\/p\/([^/]+)\/([^/]+)(?:\/(?:blob|edit|history)\/(.*))?$/,
  )
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

// An ancestor crumb is a link back to that position. Only ns/project ancestors
// are links — a sub-directory ancestor has no "open this directory" route, so it
// is rendered as plain text by the trail below, never reaching here.
//
// `activeOptions={{ exact: true }}` is required: without it, TanStack marks a link
// "active" (and adds aria-current="page") whenever its target is a PREFIX of the
// current URL — so `/p/ns/proj` would read as current on `…/blob/dir/file.md`.
// Exact matching keeps the single current marker on the final crumb only. (An
// ancestor's target is never an exact match for the deeper page it sits above.)
function CrumbLink({ crumb }: { crumb: Extract<Crumb, { kind: 'ns' | 'project' }> }) {
  const cls = styles.crumbLink
  switch (crumb.kind) {
    case 'ns':
      return (
        <Link
          to="/"
          search={{ ns: crumb.ns }}
          activeOptions={{ exact: true }}
          className={cls}
        >
          {crumb.label}
        </Link>
      )
    case 'project':
      return (
        <Link
          to="/p/$namespace/$project"
          params={{ namespace: crumb.ns, project: crumb.proj }}
          activeOptions={{ exact: true }}
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
                ) : c.kind === 'blob' ? (
                  // A sub-directory segment: there is no "open this directory"
                  // route, so render it as plain (non-navigating) text rather than
                  // a broken blob/<dir> link that would 404 as "File not found".
                  <span className={styles.crumbDir}>{c.label}</span>
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
