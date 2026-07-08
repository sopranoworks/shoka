import type { ReactNode } from 'react'
import { Link, useRouterState } from '@tanstack/react-router'
import { DndProvider, useDrop } from 'react-dnd'
import { HTML5Backend } from 'react-dnd-html5-backend'
import { titleBarStyles, Sidebar, ContentProvider, type ShellConfig } from '@shoka/web-core'
import { useShokaContentConfig } from './shokaContentConfig'
import { SidebarTrash } from './components/SidebarTrash'
import { CommandPalette } from './components/CommandPalette'
import { Toaster } from './components/Toaster'
import { NotifyBridge } from './components/NotifyBridge'
import { MoveProvider } from './lib/moveController'
import { TrashProvider, useTrashController } from './lib/trashController'
import {
  useRailSelect,
  useResetRailToExplorerOnProjectChange,
  isSettingsPath,
} from './lib/useRailSelect'

// --- Rail items (Shoka's activity bar) ------------------------------------

const SHOKA_RAIL_ITEMS: ShellConfig['railItems'] = [
  {
    id: 'explorer',
    label: 'Explorer',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <path
          d="M4 5.5h5l2 2h9v11H4z"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    id: 'search',
    label: 'Search',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <circle cx="10.5" cy="10.5" r="6" stroke="currentColor" strokeWidth="1.6" />
        <path d="M15 15l4.5 4.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      </svg>
    ),
  },
  {
    id: 'history',
    label: 'History',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <path d="M4 12a8 8 0 1 0 2.5-5.8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        <path d="M4 4v3h3" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
        <path d="M12 8v4l3 2" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    ),
  },
  {
    id: 'settings',
    label: 'Settings',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <circle cx="12" cy="12" r="3" stroke="currentColor" strokeWidth="1.6" />
        <path
          d="M12 3.5v2M12 18.5v2M20.5 12h-2M5.5 12h-2M17.5 6.5l-1.4 1.4M7.9 16.1l-1.4 1.4M17.5 17.5l-1.4-1.4M7.9 7.9 6.5 6.5"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
]

// --- Shoka breadcrumbs ----------------------------------------------------

type Crumb =
  | { label: string; kind: 'ns'; ns: string }
  | { label: string; kind: 'project'; ns: string; proj: string }
  | { label: string; kind: 'blob'; ns: string; proj: string; path: string }

function useCrumbs(): Crumb[] {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const search = useRouterState({
    select: (s) => s.location.search as { ns?: string },
  })
  const crumbs: Crumb[] = []

  const m = pathname.match(
    /^\/p\/([^/]+)\/([^/]+)(?:\/(?:(?:blob|edit|history)\/(.*)|search))?$/,
  )
  if (!m) {
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

function CrumbLink({ crumb, className }: { crumb: Extract<Crumb, { kind: 'ns' | 'project' }>; className: string }) {
  switch (crumb.kind) {
    case 'ns':
      return (
        <Link
          to="/"
          search={{ ns: crumb.ns }}
          activeOptions={{ exact: true }}
          className={className}
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
          className={className}
        >
          {crumb.label}
        </Link>
      )
  }
}

function ShokaBreadcrumbs({ styles }: { styles: Record<string, string> }) {
  const crumbs = useCrumbs()
  if (crumbs.length === 0) return null
  return (
    <>
      <span className={styles.brandChevron} aria-hidden="true">
        ›
      </span>
      <nav className={styles.crumbs} aria-label="Breadcrumb">
        {crumbs.map((c, i) => {
          const isLast = i === crumbs.length - 1
          return (
            <span key={i} className={styles.crumbItem}>
              {i > 0 && <span className={styles.sep}>/</span>}
              {isLast ? (
                <span className={styles.crumbCurrent} aria-current="page">
                  {c.label}
                </span>
              ) : c.kind === 'blob' ? (
                <span className={styles.crumbDir}>{c.label}</span>
              ) : (
                <CrumbLink crumb={c} className={styles.crumbLink} />
              )}
            </span>
          )
        })}
      </nav>
    </>
  )
}

// --- Shoka shell wrapper (DnD + trash + move providers) -------------------

function ShokaContentBridge({ children }: { children: ReactNode }) {
  const config = useShokaContentConfig()
  return <ContentProvider value={config}>{children}</ContentProvider>
}

function ShokaShellWrapper({ children }: { children: ReactNode }) {
  return (
    <MoveProvider>
      <TrashProvider>
        <DndProvider backend={HTML5Backend}>
          <ShokaContentBridge>
            {children}
          </ShokaContentBridge>
        </DndProvider>
      </TrashProvider>
    </MoveProvider>
  )
}

// --- Trash rail button (DnD drop target) ----------------------------------

interface NodeDragItem {
  id: string
  dragIds: string[]
}

function useActiveProjectRef(): { ns: string; proj: string } | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
  if (!m) return null
  return { ns: decodeURIComponent(m[1]), proj: decodeURIComponent(m[2]) }
}

function TrashRailButton({ styles }: { styles: Record<string, string> }) {
  const { items, paneOpen, togglePane, enqueuePath } = useTrashController()
  const projectRef = useActiveProjectRef()
  const [{ isOver }, dropRef] = useDrop<NodeDragItem, void, { isOver: boolean }>(
    () => ({
      accept: 'NODE',
      canDrop: () => !!projectRef,
      drop: (item) => {
        if (!projectRef) return
        const paths = item.dragIds?.length ? item.dragIds : [item.id]
        for (const path of paths) {
          void enqueuePath({
            namespace: projectRef.ns,
            project: projectRef.proj,
            path,
          })
        }
      },
      collect: (m) => ({ isOver: m.isOver() && m.canDrop() }),
    }),
    [projectRef, enqueuePath],
  )
  return (
    <button
      ref={dropRef}
      type="button"
      className={styles.trash}
      data-active={paneOpen}
      data-drop-active={isOver}
      aria-label="Trash"
      aria-pressed={paneOpen}
      title="Trash — files pending deletion (drop a file here to delete)"
      onClick={() => togglePane()}
    >
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <path
          d="M5 7h14M10 7V5h4v2M6 7l1 12h10l1-12"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
      {items.length > 0 && (
        <span className={styles.badge} aria-label={`${items.length} queued`}>
          {items.length}
        </span>
      )}
    </button>
  )
}

// --- Derive active rail from route ----------------------------------------

function deriveActiveRail(pathname: string, rail: string): string {
  const onSettings = isSettingsPath(pathname)
  return onSettings ? 'settings' : rail === 'settings' ? 'explorer' : rail
}

// --- Shoka shell config ---------------------------------------------------

export const shokaShellConfig: ShellConfig = {
  brandName: 'Shoka',
  railItems: SHOKA_RAIL_ITEMS,
  renderSidebar: (view) => <Sidebar view={view} />,
  renderSidebarExtra: () => <SidebarTrash />,
  renderRailBottom: (styles) => <TrashRailButton styles={styles} />,
  renderBreadcrumbs: (styles) => <ShokaBreadcrumbs styles={styles} />,
  renderCommandPalette: () => <CommandPalette />,
  renderToaster: () => <Toaster />,
  renderNotifyBridge: () => <NotifyBridge />,
  shellWrapper: ShokaShellWrapper,
  useRailControls: useRailSelect,
  useResetRailOnProjectChange: useResetRailToExplorerOnProjectChange,
  deriveActiveRail,
  layoutAutoSaveId: 'shoka-proto-layout',
}
