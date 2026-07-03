import { lazy, Suspense } from 'react'
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
} from '@tanstack/react-router'
import {
  Shell,
  RepoListPage,
  ProjectPage,
  BlobPage,
  HistoryPage,
  SearchPage,
} from '@shoka/web-core'
import { RouteFallback } from './components/RouteFallback'

// The editor route pulls in CodeMirror and the @codemirror/lang-* packages —
// the heaviest part of the app. Lazy-loading it (and the search route) keeps
// them out of the initial bundle: a user who only browses and reads never
// downloads the editor chunk. The read-only CodeView (BlobPage) shares the same
// chunk, so opening a code file or the editor pays the cost once.
const EditorPage = lazy(() =>
  import('./pages/EditorPage').then((m) => ({ default: m.EditorPage })),
)
const NewFilePage = lazy(() =>
  import('./pages/NewFilePage').then((m) => ({ default: m.NewFilePage })),
)
// The Settings view's right pane (B-28 stage 3) — the gear rail mode's content,
// lazy since it pulls the user-management + OAuth connections screens.
const SettingsPage = lazy(() =>
  import('@shoka/web-core/pages/SettingsPage').then((m) => ({
    default: m.SettingsPage,
  })),
)
// The admin "Deleted files" view (B-28, the 2026-06-18 deleted-log directive):
// lists a project's deleted files and revives one forward-only. Lazy — it is an
// admin-only power-user surface, off the initial bundle.
const DeletedPage = lazy(() =>
  import('./pages/DeletedPage').then((m) => ({ default: m.DeletedPage })),
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
// The optional ?highlight= search triggers the in-view find bar with the given
// query (used by the sidebar search → click-to-view flow).
interface BlobSearch {
  highlight?: string
}

const blobRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/blob/$',
  validateSearch: (search: Record<string, unknown>): BlobSearch => {
    const h = typeof search.highlight === 'string' && search.highlight ? search.highlight : undefined
    return h ? { highlight: h } : {}
  },
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
  component: SearchPage,
})

// "/p/$namespace/$project/new" path-less new-file editor (session 4). Additive
// and parallel to /edit/$ — an empty editor where the path is chosen at Save.
// The optional ?in= search carries the directory the create was launched from
// (B-31 fix #3/#4), so the Save-path dialog can be prefilled with that location
// (sibling-ready); the path stays fully editable to any nested target.
interface NewFileSearch {
  in?: string
}

const newFileRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/new',
  validateSearch: (search: Record<string, unknown>): NewFileSearch => {
    const dir = typeof search.in === 'string' && search.in ? search.in : undefined
    return dir ? { in: dir } : {}
  },
  component: lazyRoute(NewFilePage),
})

// "/admin/connections" now REDIRECTS to the OAuth connections Settings item
// (`/settings?item=oauth`) — the screen's real home as of the OAuth-settings-item
// work. The old path is kept as a redirect so existing links/bookmarks never break.
// Authorization is unchanged (server-side admin gate on OAUTH_*; the Settings item is
// super-user-only via the registry filter).
const connectionsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/admin/connections',
  beforeLoad: () => {
    throw redirect({ to: '/settings', search: { item: 'oauth' } })
  },
})

// Settings (B-28 stage 3): the gear rail mode's content. The selected item lives in
// `?item=`. Project-scoped (`/p/$ns/$proj/settings`) keeps the project in the URL so
// the sidebar file tree stays mounted (no remount/collapse) while in Settings; the
// global form (`/settings`) is reachable off-project (e.g. from the repo list).
interface SettingsSearch {
  item?: string
}
function validateSettingsSearch(search: Record<string, unknown>): SettingsSearch {
  const item = typeof search.item === 'string' && search.item ? search.item : undefined
  return item ? { item } : {}
}

const projectSettingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/settings',
  validateSearch: validateSettingsSearch,
  component: lazyRoute(SettingsPage),
})

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings',
  validateSearch: validateSettingsSearch,
  component: lazyRoute(SettingsPage),
})

// "/p/$namespace/$project/deleted" the admin deleted-files view (B-28). Lists the
// project's currently-deleted files and revives one. Admin-only (server gate
// authoritative; the page also hides for a non-admin).
const deletedRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/deleted',
  component: lazyRoute(DeletedPage),
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
  projectSettingsRoute,
  settingsRoute,
  deletedRoute,
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
  type BlobSearch,
  type IndexSearch,
  type SearchSearch,
  type HistorySearch,
  type NewFileSearch,
}
