import { useCallback } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import type { RailView } from '../components/ActivityRail'

// Stable references so passing them as props never churns the rail.
const NONE_DISABLED: RailView[] = []
// Off a project route (the admin screens) Search and History have no meaningful
// action — they would mean search/history over the admin context (e.g. OAuth
// tokens), which is unbuilt — so they are disabled rather than routed anywhere.
// Explorer stays enabled there (it returns to the project list).
const ADMIN_DISABLED: RailView[] = ['search', 'history']

export interface RailControls {
  onSelect: (v: RailView) => void
  disabledItems: RailView[]
}

// The activity-rail behaviour. On a project route (pathname under `/p/`) every
// rail item switches the sidebar pane (Explorer/Search/History) as it always
// has. Off a project route — the admin screens (`/admin/*`), and "/" itself —
// only Explorer is meaningful: it routes to "/" (the project list), so the rail
// is never a dead button; Search and History are disabled (genuinely inert).
export function useRailSelect(
  setRail: (v: RailView) => void,
  setSidebarOpen: (open: boolean) => void,
): RailControls {
  const navigate = useNavigate()
  const onProjectRoute = useRouterState({
    select: (s) => s.location.pathname.startsWith('/p/'),
  })
  const onSelect = useCallback(
    (v: RailView) => {
      if (!onProjectRoute) {
        // Only Explorer is enabled off a project route (Search/History are
        // disabled and never reach here); it returns to the project list.
        void navigate({ to: '/' })
        return
      }
      setRail(v)
      setSidebarOpen(true)
    },
    [onProjectRoute, navigate, setRail, setSidebarOpen],
  )
  return {
    onSelect,
    disabledItems: onProjectRoute ? NONE_DISABLED : ADMIN_DISABLED,
  }
}
