import type { FileNode, TreeNode, ProjectInfo } from './types'

// Map the backend's FileNode tree to react-arborist's TreeNode shape. A file's
// id is its full path, so the tree's `selection` can be driven by the URL path.
export function toTreeNodes(nodes: FileNode[]): TreeNode[] {
  return nodes.map((n) => ({
    id: n.path,
    name: n.name,
    path: n.path,
    isFile: !n.isDir,
    children: n.isDir ? toTreeNodes(n.children ?? []) : undefined,
  }))
}

// Flatten a FileNode tree to the list of file paths (quick-open and file counts).
export function flattenFilePaths(nodes: FileNode[]): string[] {
  const out: string[] = []
  const walk = (ns: FileNode[]) => {
    for (const n of ns) {
      if (n.isDir) walk(n.children ?? [])
      else out.push(n.path)
    }
  }
  walk(nodes)
  return out
}

// Ancestor directory paths of a file path, for expand-to-active in the tree.
// "a/b/c.md" -> ["a", "a/b"].
export function ancestorDirs(filePath: string): string[] {
  const segs = filePath.split('/').filter(Boolean)
  const dirs: string[] = []
  let accum = ''
  for (let i = 0; i < segs.length - 1; i++) {
    accum = accum ? `${accum}/${segs[i]}` : segs[i]
    dirs.push(accum)
  }
  return dirs
}

// Unique namespaces present in the project list, sorted.
export function namespacesOf(projects: ProjectInfo[]): string[] {
  return [...new Set(projects.map((p) => p.namespace))].sort()
}
