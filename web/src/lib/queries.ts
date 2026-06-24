import { useQuery, useQueries } from '@tanstack/react-query'
import { wsClient } from '@shoka/web-core'
import { flattenFilePaths } from './tree'
import type {
  FileNode,
  FileContent,
  HistoryPayload,
  FileAtContent,
  FileDiff,
} from '@shoka/web-core'
import { useProjectsQuery } from '@shoka/web-core'

// The reusable read-queries (OAuth management + the project list) live in
// @shoka/web-core; re-export them so existing app callers keep importing from
// '../lib/queries' unchanged.
export {
  OAUTH_CONNECTIONS_KEY,
  OAUTH_DOMAINS_KEY,
  OAUTH_CLIENTS_KEY,
  useConnectionsQuery,
  useDomainsQuery,
  useConfidentialClientsQuery,
  useProjectsQuery,
} from '@shoka/web-core'

// Document-tree / file / history queries — the app's read surface. All data is read
// over the /ws/ui request/response surface (wsClient) and flows through TanStack Query,
// so navigation is cache-driven and instant on revisit. Cache keys: ['projects'],
// ['tree', ns, project], ['file', ns, project, path].

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

// --- History (B-31 phase 2) -------------------------------------------------
// All three read over the same lock-free storage reads as the rest of the app.
// Keys: ['history', ns, project, path], ['file-at', ns, project, path, hash],
// ['diff', ns, project, path, from, to]. Immutable-commit reads, so once fetched
// they need no invalidation (a commit's content never changes).

// The commit list for one file (subject + commit date + committer, no file list).
export function useHistoryQuery(
  namespace: string,
  project: string,
  path: string,
) {
  return useQuery({
    queryKey: ['history', namespace, project, path],
    enabled: path !== '',
    queryFn: () =>
      wsClient().request<HistoryPayload>('GET_HISTORY', {
        namespace,
        projectName: project,
        path,
      }),
  })
}

// A file's content at one explicit commit (read-only version view).
export function useFileAtQuery(
  namespace: string,
  project: string,
  path: string,
  hash: string,
) {
  return useQuery({
    queryKey: ['file-at', namespace, project, path, hash],
    enabled: path !== '' && hash !== '',
    queryFn: () =>
      wsClient().request<FileAtContent>('GET_FILE_AT', {
        namespace,
        projectName: project,
        path,
        hash,
      }),
  })
}

// The structured diff of one file between two explicit commits.
export function useDiffQuery(
  namespace: string,
  project: string,
  path: string,
  fromHash: string,
  toHash: string,
) {
  return useQuery({
    queryKey: ['diff', namespace, project, path, fromHash, toHash],
    enabled: path !== '' && fromHash !== '' && toHash !== '',
    queryFn: () =>
      wsClient().request<FileDiff>('GET_DIFF', {
        namespace,
        projectName: project,
        path,
        fromHash,
        toHash,
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
