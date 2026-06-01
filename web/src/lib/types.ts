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

// The READ_FILE response payload. `etag` is the sha256 of the content
// (added by the ws-ui-versioning precursor); the editor uses it as the
// optimistic-concurrency `if_match` on the next SAVE_FILE.
export interface FileContent {
  path: string
  content: string
  etag: string
}

// The SAVE_ACK response payload: the new etag becomes the buffer's next
// if_match. The CONFLICT payload carries the server's current etag so a
// "Force overwrite" can save against it.
export interface SaveAck {
  path: string
  status: string
  etag: string
}
export interface ConflictPayload {
  path: string
  current_etag: string
  message: string
}

// One SEARCH_RESULT match (mirrors Go's storage.SearchMatch). The snippet is a
// short context window around the match, not a line; the backend does not carry
// a line number, so the result UI shows path + snippet and navigates to the
// blob view (no scroll-to-line). Search is project-scoped.
export interface SearchMatch {
  path: string
  snippet?: string
}

// react-arborist node, derived from FileNode via lib/tree.
export interface TreeNode {
  id: string // unique within a project; for files this is the file path
  name: string // leaf label (last path segment)
  path: string // full slash-joined path (directories included)
  isFile: boolean
  children?: TreeNode[]
}
