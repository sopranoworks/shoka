import { useQuery } from '@tanstack/react-query'
import { wsClient } from '@shoka/web-core'
import type { SearchMatch } from '@shoka/web-core'

// Project-scoped full-text/filename search over /ws/ui SEARCH_FILES (session 4).
// The backend tool (storage.SearchFiles, the same one the MCP search_files tool
// uses) searches one project at a time and returns {path, snippet} matches with
// no line number — so this is deliberately not a cross-project search.

interface SearchResultPayload {
  matches: SearchMatch[]
}

// searchFiles issues one SEARCH_FILES request and returns the matches. The
// imperative layer (mirrors lib/fileOps) so it is unit-testable without a hook.
export async function searchFiles(
  namespace: string,
  project: string,
  query: string,
): Promise<SearchMatch[]> {
  const res = await wsClient().request<SearchResultPayload>('SEARCH_FILES', {
    namespace,
    projectName: project,
    query,
    // "both" (filename + content) is the storage default; passed explicitly so
    // the wire request is self-describing.
    search_in: 'both',
  })
  return res.matches ?? []
}

// useSearchQuery wraps searchFiles in TanStack Query. Cache key
// ['search', ns, project, query] keeps each query's results distinct and
// reload-stable; an empty (trimmed) query is disabled — no request, no results.
export function useSearchQuery(
  namespace: string,
  project: string,
  query: string,
) {
  const q = query.trim()
  return useQuery({
    queryKey: ['search', namespace, project, q],
    enabled: q !== '',
    queryFn: () => searchFiles(namespace, project, q),
  })
}
