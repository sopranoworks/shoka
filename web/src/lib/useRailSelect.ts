import { useCallback, useEffect } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from '../components/ActivityRail'

// Stable references so passing them as props never churns the rail.
const NONE_DISABLED: RailView[] = []
// Off a project route — the project-selection screen ("/") and the admin screens
// (`/admin/*`) — no rail mode is meaningful (there is no project to explore,
// search, or view history for), so ALL three items are disabled (genuinely inert).
const NO_PROJECT_DISABLED: RailView[] = ['explorer', 'search', 'history']

// The namespace/project, and (if any) the file shown by the current file-bearing
// route, parsed from the pathname — so History can open the active file's history
// directly. The splat is kept raw (matching how the route carries it).
function parseProjectFile(pathname: string): {
  ns: string
  proj: string
  path: string | null
} | null {
  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)(?:\/(?:blob|edit|history)\/(.*))?$/)
  if (!m) return null
  return {
    ns: decodeURIComponent(m[1]),
    proj: decodeURIComponent(m[2]),
    path: m[3] ? decodeURIComponent(m[3]) : null,
  }
}

export interface RailControls {
  onSelect: (v: RailView) => void
  disabledItems: RailView[]
}

// The activity-rail behaviour. The rail is a consistent TOGGLE: clicking the
// active item while its pane is open closes the sidebar; clicking any other item
// (or a closed pane) opens that pane. History additionally opens the active file's
// history in the right pane (the 4fc366f behaviour) — but only on open, never when
// toggling closed. Off a project route ("/" and `/admin/*`) every item is disabled
// (no mode is meaningful before a project is chosen), so onSelect is never reached
// there.
export function useRailSelect(
  rail: RailView,
  sidebarOpen: boolean,
  setRail: (v: RailView) => void,
  setSidebarOpen: (open: boolean) => void,
): RailControls {
  const navigate = useNavigate()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const onProjectRoute = pathname.startsWith('/p/')
  const onHistoryRoute = /^\/p\/[^/]+\/[^/]+\/history(\/|$)/.test(pathname)
  const onSelect = useCallback(
    (v: RailView) => {
      if (!onProjectRoute) return // all items disabled off a project route
      // Explorer leaving History: symmetric to File→History — return the content
      // to the SAME file's file view (`/blob/<file>`), not just switch the rail.
      // Runs before the toggle check so it also corrects a reload-desync (rail
      // 'explorer' on a history URL). No file selected → the project root.
      if (v === 'explorer' && onHistoryRoute) {
        const ref = parseProjectFile(pathname)
        setRail('explorer')
        setSidebarOpen(true)
        if (ref?.path) {
          void navigate({
            to: '/p/$namespace/$project/blob/$',
            params: { namespace: ref.ns, project: ref.proj, _splat: ref.path },
          })
        } else if (ref) {
          void navigate({
            to: '/p/$namespace/$project',
            params: { namespace: ref.ns, project: ref.proj },
          })
        }
        return
      }
      // Toggle: clicking the already-open active pane closes the sidebar.
      if (v === rail && sidebarOpen) {
        setSidebarOpen(false)
        return
      }
      setRail(v)
      setSidebarOpen(true)
      if (v === 'history') {
        const ref = parseProjectFile(pathname)
        if (ref) {
          void navigate({
            to: '/p/$namespace/$project/history/$',
            params: {
              namespace: ref.ns,
              project: ref.proj,
              _splat: ref.path ?? '',
            },
          })
        }
      }
    },
    [
      onProjectRoute,
      onHistoryRoute,
      rail,
      sidebarOpen,
      pathname,
      navigate,
      setRail,
      setSidebarOpen,
    ],
  )
  return {
    onSelect,
    disabledItems: onProjectRoute ? NONE_DISABLED : NO_PROJECT_DISABLED,
  }
}

// Selecting a project (or switching projects) defaults the rail to Explorer (the
// file view) — one wouldn't pick a project to immediately Search/History. Keyed on
// the project (ns/proj), so navigating among a project's files/history keeps the
// current rail; only a project-key change (incl. arriving from the no-project
// screen) resets it. Does not fight URL-as-state — it reacts to the project, not
// every navigation.
export function useResetRailToExplorerOnProjectChange(
  setRail: (v: RailView) => void,
): void {
  const projectKey = useRouterState({
    select: (s) => {
      const m = s.location.pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
      return m ? `${m[1]}/${m[2]}` : null
    },
  })
  useEffect(() => {
    if (projectKey) setRail('explorer')
  }, [projectKey, setRail])
}
