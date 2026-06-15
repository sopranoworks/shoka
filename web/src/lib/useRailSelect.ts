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

// The activity-rail behaviour. On a project route (pathname under `/p/`):
//  - Explorer/Search switch the sidebar pane as they always have;
//  - History switches to the History pane (the tree stays) AND opens the active
//    file's history in the right pane directly (no "View history →" cushion);
//    with no file selected it opens the history route with an empty path, whose
//    page shows a quiet "select a file" placeholder.
// Off a project route — the admin screens (`/admin/*`) and "/" — only Explorer is
// meaningful (routes to "/"); Search and History are disabled (genuinely inert).
export function useRailSelect(
  setRail: (v: RailView) => void,
  setSidebarOpen: (open: boolean) => void,
): RailControls {
  const navigate = useNavigate()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const onProjectRoute = pathname.startsWith('/p/')
  const onSelect = useCallback(
    (v: RailView) => {
      if (!onProjectRoute) {
        // Only Explorer is enabled off a project route (Search/History are
        // disabled and never reach here); it returns to the project list.
        void navigate({ to: '/' })
        return
      }
      if (v === 'history') {
        const ref = parseProjectFile(pathname)
        setRail('history')
        setSidebarOpen(true)
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
        return
      }
      setRail(v)
      setSidebarOpen(true)
    },
    [onProjectRoute, pathname, navigate, setRail, setSidebarOpen],
  )
  return {
    onSelect,
    disabledItems: onProjectRoute ? NONE_DISABLED : ADMIN_DISABLED,
  }
}
