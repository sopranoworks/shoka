import { useCallback } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from '../components/ActivityRail'

// The activity-rail select handler. On a project route (pathname under `/p/`) the
// rail switches the sidebar pane (Explorer/Search/History) as it always has. On a
// route with NO project in context — the admin screens (`/admin/*`), where the
// Explorer/Search/History panes have nothing to show and clicking them was a
// silent no-op (B-31 fix #1) — it routes to "/" (the project list) instead, so the
// rail is never a dead button. Home ("/") falls in the no-project branch too,
// where routing to "/" is a harmless self-navigation.
export function useRailSelect(
  setRail: (v: RailView) => void,
  setSidebarOpen: (open: boolean) => void,
): (v: RailView) => void {
  const navigate = useNavigate()
  const onProjectRoute = useRouterState({
    select: (s) => s.location.pathname.startsWith('/p/'),
  })
  return useCallback(
    (v: RailView) => {
      if (!onProjectRoute) {
        void navigate({ to: '/' })
        return
      }
      setRail(v)
      setSidebarOpen(true)
    },
    [onProjectRoute, navigate, setRail, setSidebarOpen],
  )
}
