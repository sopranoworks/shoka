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

// "/" repository list, with a typed search param ?ns= for the namespace filter.
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

const routeTree = rootRoute.addChildren([
  indexRoute,
  projectRoute,
  blobRoute,
  editRoute,
  searchRoute,
])

export const router = createRouter({
  routeTree,
  defaultPreload: 'intent',
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
  editRoute,
  searchRoute,
  type IndexSearch,
  type SearchSearch,
}
