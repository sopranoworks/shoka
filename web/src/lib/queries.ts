import { useQuery, useQueries } from '@tanstack/react-query'
import { wsClient } from './wsClient'
import { flattenFilePaths } from './tree'
import { listConnections } from './oauthOps'
import type { ProjectInfo, FileNode, FileContent } from './types'

// The OAuth connections list query key — invalidated after a revoke so the list
// reflects the store. There is no oauth NOTIFY (the store emits none), so a
// live-refresh is manual: invalidate-on-revoke is the floor, plus an explicit
// Refresh action on the management view (a NOTIFY-driven live list is a possible
// later (b)-store enhancement, out of scope for (c)).
export const OAUTH_CONNECTIONS_KEY = ['oauth-connections'] as const

// The live OAuth/MCP connections, for the admin management view. `enabled` gates
// the fetch on the admin predicate so a non-admin never even issues OAUTH_LIST.
export function useConnectionsQuery(enabled = true) {
  return useQuery({
    queryKey: OAUTH_CONNECTIONS_KEY,
    enabled,
    queryFn: () => listConnections(),
  })
}

// All data is read over the /ws/ui request/response surface (lib/wsClient) and
// flows through TanStack Query, so navigation is cache-driven and instant on
// revisit. Cache keys: ['projects'], ['tree', ns, project], ['file', ns,
// project, path]. Invalidation on NOTIFY is session 2; here data is fetched
// once per key and refreshed by a full reload.

export function useProjectsQuery() {
  return useQuery({
    queryKey: ['projects'],
    queryFn: () => wsClient().request<ProjectInfo[]>('GET_PROJECTS', {}),
  })
}

export function useTreeQuery(namespace: string, project: string) {
  return useQuery({
    queryKey: ['tree', namespace, project],
    queryFn: async () =>
      (await wsClient().request<FileNode[] | null>('GET_TREE', {
        namespace,
        projectName: project,
      })) ?? [],
  })
}

export function useFileQuery(namespace: string, project: string, path: string) {
  return useQuery({
    queryKey: ['file', namespace, project, path],
    enabled: path !== '',
    queryFn: () =>
      wsClient().request<FileContent>('READ_FILE', {
        namespace,
        projectName: project,
        path,
      }),
  })
}

export interface GlobalFile {
  namespace: string
  project: string
  path: string
}

// Every file across every project, for global quick-open (palette §1.5). Lazily
// enabled (only when the quick-open page is open) so the N GET_TREE round-trips
// happen on demand; results share the ['tree', ns, project] cache with the
// sidebar, so an already-browsed project's files are already present.
export function useAllProjectFiles(enabled: boolean): GlobalFile[] {
  const { data: projects = [] } = useProjectsQuery()

  const results = useQueries({
    queries: projects.map((p) => ({
      queryKey: ['tree', p.namespace, p.name],
      queryFn: async () =>
        (await wsClient().request<FileNode[] | null>('GET_TREE', {
          namespace: p.namespace,
          projectName: p.name,
        })) ?? [],
      enabled,
    })),
  })

  const files: GlobalFile[] = []
  results.forEach((r, i) => {
    const p = projects[i]
    if (!p || !r.data) return
    for (const path of flattenFilePaths(r.data)) {
      files.push({ namespace: p.namespace, project: p.name, path })
    }
  })
  return files
}
