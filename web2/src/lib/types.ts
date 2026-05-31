// Shapes mirroring the Shoka /ws/ui request/response surface
// (internal/ui/manager.go). The Web UI reads all data over that WebSocket.

// Health state of a project, from GET_PROJECTS; drives the status badge.
export type ProjectState = 'healthy' | 'corrupted' | 'dangerous' | (string & {})

// One project entry in the GET_PROJECTS response.
export interface ProjectInfo {
  namespace: string
  name: string
  state: ProjectState
}

// A file-tree node in the GET_TREE response (mirrors Go's ui.FileNode).
// Directories carry children; files do not.
export interface FileNode {
  name: string
  path: string
  isDir: boolean
  children?: FileNode[]
}

// The READ_FILE response payload.
export interface FileContent {
  path: string
  content: string
}

// react-arborist node, derived from FileNode via lib/tree.
export interface TreeNode {
  id: string // unique within a project; for files this is the file path
  name: string // leaf label (last path segment)
  path: string // full slash-joined path (directories included)
  isFile: boolean
  children?: TreeNode[]
}
