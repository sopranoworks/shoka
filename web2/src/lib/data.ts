import rawData from '../data/mock-data.json'
import type { MockData, MockProject, MockFile, TreeNode } from './types'

// The JSON is bundled at build time; no runtime fetch.
export const mockData = rawData as MockData

export function getProject(
  namespace: string,
  name: string,
): MockProject | undefined {
  return mockData.projects.find(
    (p) => p.namespace === namespace && p.name === name,
  )
}

export function getFile(
  project: MockProject | undefined,
  path: string,
): MockFile | undefined {
  if (!project) return undefined
  return project.files.find((f) => f.path === path)
}

export function projectKey(p: { namespace: string; name: string }): string {
  return `${p.namespace}/${p.name}`
}

/**
 * Build a nested tree from flat `/`-separated file paths.
 * Directories are synthesized from path segments; files are leaves.
 * Sorted: directories first, then files, each alphabetical.
 */
export function buildTree(files: MockFile[]): TreeNode[] {
  const root: TreeNode = {
    id: '__root__',
    name: '',
    path: '',
    isFile: false,
    children: [],
  }

  for (const file of files) {
    const segments = file.path.split('/').filter(Boolean)
    let cursor = root
    let accum = ''
    segments.forEach((seg, idx) => {
      accum = accum ? `${accum}/${seg}` : seg
      const isLeaf = idx === segments.length - 1
      cursor.children ??= []
      let next = cursor.children.find((c) => c.path === accum)
      if (!next) {
        next = {
          id: accum,
          name: seg,
          path: accum,
          isFile: isLeaf,
          children: isLeaf ? undefined : [],
        }
        cursor.children.push(next)
      }
      cursor = next
    })
  }

  const sortRec = (nodes: TreeNode[]): TreeNode[] => {
    nodes.sort((a, b) => {
      if (a.isFile !== b.isFile) return a.isFile ? 1 : -1
      return a.name.localeCompare(b.name)
    })
    for (const n of nodes) if (n.children) sortRec(n.children)
    return nodes
  }

  return sortRec(root.children ?? [])
}

// Flat list of file paths for fuzzy quick-open.
export function allFilePaths(project: MockProject | undefined): string[] {
  if (!project) return []
  return project.files.map((f) => f.path)
}
