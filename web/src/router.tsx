import { lazy, Suspense } from 'react'
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from '@tanstack/react-router'
import { Shell } from './components/Shell'
import { RouteFallback } from './components/RouteFallback'
import { RepoListPage } from './pages/RepoListPage'
import { ProjectPage } from './pages/ProjectPage'
import { BlobPage } from './pages/BlobPage'
import { HistoryPage } from './pages/HistoryPage'

// The editor route pulls in CodeMirror and the @codemirror/lang-* packages —
// the heaviest part of the app. Lazy-loading it (and the search route) keeps
// them out of the initial bundle: a user who only browses and reads never
// downloads the editor chunk. The read-only CodeView (BlobPage) shares the same
// chunk, so opening a code file or the editor pays the cost once.
const EditorPage = lazy(() =>
  import('./pages/EditorPage').then((m) => ({ default: m.EditorPage })),
)
const SearchPage = lazy(() =>
  import('./pages/SearchPage').then((m) => ({ default: m.SearchPage })),
)
const NewFilePage = lazy(() =>
  import('./pages/NewFilePage').then((m) => ({ default: m.NewFilePage })),
)
// The OAuth connection management view (B-39 (c)) — a global, administrator-only
// admin screen, lazy-loaded since it is rarely opened.
const ConnectionsPage = lazy(() =>
  import('./pages/ConnectionsPage').then((m) => ({
    default: m.ConnectionsPage,
  })),
)

// Wrap a lazily-loaded page in a Suspense boundary with the delayed fallback.
function lazyRoute(Page: React.ComponentType) {
  return function LazyRouteComponent() {
    return (
      <Suspense fallback={<RouteFallback />}>
        <Page />
      </Suspense>
    )
  }
}

// Root renders the persistent docked shell. The shell never unmounts; only
// the <Outlet/> inside its content region swaps on navigation.
const rootRoute = createRootRoute({
  component: () => (
    <Shell>
      <Outlet />
    </Shell>
  ),
})

// "/" project list, with a typed search param ?ns= for the namespace filter.
interface IndexSearch {
  ns?: string
}

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  validateSearch: (search: Record<string, unknown>): IndexSearch => {
    const ns = typeof search.ns === 'string' ? search.ns : undefined
    return ns ? { ns } : {}
  },
  component: RepoListPage,
})

// "/p/$namespace/$project" project view.
const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project',
  component: ProjectPage,
})

// "/p/$namespace/$project/blob/$" file view (splat captures the rest as path).
// Viewing is the primary surface; the edit route below is the session-3 editor.
const blobRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/blob/$',
  component: BlobPage,
})

// "/p/$namespace/$project/history/$" per-file History view (B-31 phase 2): the
// file's commit list → a chosen version's content → a diff of two versions. The
// selected version (?at=), the diff pair (?from=/?to=), and the panel mode
// (?mode=version|diff) live in the URL so reload/back/forward restore the view.
interface HistorySearch {
  at?: string
  from?: string
  to?: string
  mode?: 'version' | 'diff'
}

const historyRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/history/$',
  validateSearch: (search: Record<string, unknown>): HistorySearch => {
    const str = (v: unknown) => (typeof v === 'string' && v ? v : undefined)
    const mode = search.mode === 'version' || search.mode === 'diff' ? search.mode : undefined
    return {
      ...(str(search.at) ? { at: str(search.at) } : {}),
      ...(str(search.from) ? { from: str(search.from) } : {}),
      ...(str(search.to) ? { to: str(search.to) } : {}),
      ...(mode ? { mode } : {}),
    }
  },
  component: HistoryPage,
})

// "/p/$namespace/$project/edit/$" editor for an existing file (session 3). Same
// splat convention as blob, so view↔edit is a navigation between sibling routes.
const editRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/edit/$',
  component: lazyRoute(EditorPage),
})

// "/p/$namespace/$project/search" project-scoped full-text/filename search
// (session 4). The query lives in the URL (?q=) so results are deep-linkable,
// reload-safe, and Back/Forward navigable. Search is scoped to one project,
// matching the backend SEARCH_FILES capability and design v3 §6.5's reserved
// per-project search route.
interface SearchSearch {
  q?: string
}

const searchRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/search',
  validateSearch: (search: Record<string, unknown>): SearchSearch => {
    const q = typeof search.q === 'string' ? search.q : undefined
    return q ? { q } : {}
  },
  component: lazyRoute(SearchPage),
})

// "/p/$namespace/$project/new" path-less new-file editor (session 4). Additive
// and parallel to /edit/$ — an empty editor where the path is chosen at Save.
const newFileRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/new',
  component: lazyRoute(NewFilePage),
})

// "/admin/connections" administrator-only OAuth connection management (B-39 (c)).
// The /admin prefix encodes the authorization boundary in the URL and is the
// future attach point for a route-level admin gate — but it does NOT itself
// secure anything: the authoritative gate is the server-side admin predicate on
// OAUTH_LIST/OAUTH_REVOKE (the page also hides for non-admins via useIsAdmin).
const connectionsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/admin/connections',
  component: lazyRoute(ConnectionsPage),
})

const routeTree = rootRoute.addChildren([
  indexRoute,
  projectRoute,
  blobRoute,
  historyRoute,
  editRoute,
  searchRoute,
  newFileRoute,
  connectionsRoute,
])

export const router = createRouter({
  routeTree,
  defaultPreload: 'intent',
  // Scroll-position restoration (session 4, closing session 1's open finding).
  // The cache persists in sessionStorage and is keyed by the location href, so a
  // reload, Back/Forward, or deep-link returns to the same scroll offset. Page
  // scroll containers opt in via data-scroll-restoration-id (the app scrolls
  // inner panels, not the window).
  scrollRestoration: true,
  getScrollRestorationKey: (location) => location.href,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export {
  rootRoute,
  indexRoute,
  projectRoute,
  blobRoute,
  historyRoute,
  editRoute,
  searchRoute,
  newFileRoute,
  connectionsRoute,
  type IndexSearch,
  type SearchSearch,
  type HistorySearch,
}
