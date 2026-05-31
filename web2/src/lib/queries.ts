import { useQuery } from '@tanstack/react-query'
import { mockData, getProject, getFile, buildTree } from './data'
import type { MockProject } from './types'

// All queries resolve from the bundled mock data, but they flow through
// TanStack Query so navigation is cache-driven and instant on revisit.

export function useProjectsQuery() {
  return useQuery({
    queryKey: ['projects'],
    queryFn: async () => mockData.projects,
    staleTime: Infinity,
  })
}

export function useProjectQuery(namespace: string, project: string) {
  return useQuery({
    queryKey: ['project', namespace, project],
    queryFn: async (): Promise<MockProject> => {
      const p = getProject(namespace, project)
      if (!p) throw new Error(`Unknown project: ${namespace}/${project}`)
      return p
    },
    staleTime: Infinity,
  })
}

export function useTreeQuery(namespace: string, project: string) {
  return useQuery({
    queryKey: ['tree', namespace, project],
    queryFn: async () => {
      const p = getProject(namespace, project)
      return buildTree(p?.files ?? [])
    },
    staleTime: Infinity,
  })
}

export function useFileQuery(
  namespace: string,
  project: string,
  path: string,
) {
  return useQuery({
    queryKey: ['file', namespace, project, path],
    queryFn: async () => {
      const p = getProject(namespace, project)
      const f = getFile(p, path)
      if (!f) throw new Error(`Unknown file: ${path}`)
      return f
    },
    staleTime: Infinity,
  })
}
