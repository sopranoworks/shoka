import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from '@tanstack/react-router'
import { Shell } from './components/Shell'
import { RepoListPage } from './pages/RepoListPage'
import { ProjectPage } from './pages/ProjectPage'
import { BlobPage } from './pages/BlobPage'
import { EditorPage } from './pages/EditorPage'
import { SearchPage } from './pages/SearchPage'

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
  component: EditorPage,
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
