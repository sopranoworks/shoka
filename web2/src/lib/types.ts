// Shapes for the bundled mock data. The JSON is imported (never fetched).
export interface MockFile {
  path: string
  content: string
}

export interface MockProject {
  namespace: string
  name: string
  files: MockFile[]
}

export interface MockData {
  namespaces: string[]
  projects: MockProject[]
}

// File-tree node consumed by react-arborist.
export interface TreeNode {
  id: string // unique within a project; for files this is the file path
  name: string // leaf label (last path segment)
  path: string // full slash-joined path (dirs included)
  isFile: boolean
  children?: TreeNode[]
}
