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
import { EditPage } from './pages/EditPage'

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
const blobRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/blob/$',
  component: BlobPage,
})

// "/p/$namespace/$project/edit/$" edit view (splat path).
const editRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/p/$namespace/$project/edit/$',
  component: EditPage,
})

const routeTree = rootRoute.addChildren([
  indexRoute,
  projectRoute,
  blobRoute,
  editRoute,
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
  type IndexSearch,
}
