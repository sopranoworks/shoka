import { useCallback, useEffect } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from '../components/ActivityRail'

// Stable references so passing them as props never churns the rail.
const NONE_DISABLED: RailView[] = []
// Off a project route — the project-selection screen ("/") and the admin screens
// (`/admin/*`) — Explorer/Search/History are not meaningful (no project to explore,
// search, or view history for), so they are disabled. Settings is NOT disabled: it is
// a GLOBAL view (its items, e.g. user management, are server-level), available
// everywhere (B-28 stage 3).
const NO_PROJECT_DISABLED: RailView[] = ['explorer', 'search', 'history']

// isSettingsPath reports whether a pathname is a settings route — global (/settings) or
// project-scoped (/p/ns/proj/settings). The Shell DERIVES the rail's "settings" active state from
// this (single source of truth), so the gear is active exactly when the settings view is shown and
// can never drift (e.g. it clears the moment "Shoka"/home navigates away).
export function isSettingsPath(pathname: string): boolean {
  return (
    /^\/settings(\/|$)/.test(pathname) ||
    /^\/p\/[^/]+\/[^/]+\/settings(\/|$)/.test(pathname)
  )
}

// parseProjectPrefix extracts ns/proj from any "/p/<ns>/<proj>..." path (including
// the settings sub-route), where parseProjectFile (which requires a blob/edit/history
// tail) does not match.
function parseProjectPrefix(pathname: string): { ns: string; proj: string } | null {
  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
  if (!m) return null
  return { ns: decodeURIComponent(m[1]), proj: decodeURIComponent(m[2]) }
}

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
  const onSettingsRoute = isSettingsPath(pathname)
  const onSelect = useCallback(
    (v: RailView) => {
      // Settings is global (available everywhere); the other modes need a project.
      if (v !== 'settings' && !onProjectRoute) return

      // Settings: navigate to the Settings route — project-scoped when in a project (so the file
      // tree stays mounted in the sidebar — no remount/collapse on return), global otherwise. The
      // rail's "settings" active state is DERIVED from the route (Shell), so we do NOT set a
      // separate rail state here (which is exactly what used to leave the gear stuck active after
      // leaving Settings). Toggling the open Settings pane (already on a settings route) closes it.
      if (v === 'settings') {
        if (onSettingsRoute && sidebarOpen) {
          setSidebarOpen(false)
          return
        }
        setSidebarOpen(true)
        const ref = parseProjectPrefix(pathname)
        if (ref) {
          void navigate({
            to: '/p/$namespace/$project/settings',
            params: { namespace: ref.ns, project: ref.proj },
          })
        } else {
          void navigate({ to: '/settings' })
        }
        return
      }

      // Explorer leaving Settings → back to the project view (symmetric to leaving
      // History): return the content to the project root, or the repo list if global.
      if (v === 'explorer' && onSettingsRoute) {
        setRail('explorer')
        setSidebarOpen(true)
        const ref = parseProjectPrefix(pathname)
        if (ref) {
          void navigate({ to: '/p/$namespace/$project', params: { namespace: ref.ns, project: ref.proj } })
        } else {
          void navigate({ to: '/' })
        }
        return
      }
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
      onSettingsRoute,
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
