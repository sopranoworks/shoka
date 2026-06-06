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

// The MOVE_ACK response payload (mirrors Go's ui.MoveAckPayload). A move is a
// pure path change (B-33): new_etag is the moved file's etag (equal to the
// source's, since content is unchanged), and links_rewritten is ALWAYS 0 — the
// field is retained for forward compatibility but the UI never surfaces it.
export interface MoveAck {
  source_path: string
  target_path: string
  new_etag: string
  links_rewritten: number
}

// One SEARCH_RESULT match (mirrors Go's storage.SearchMatch). The snippet is a
// short context window around the match, not a line; the backend does not carry
// a line number, so the result UI shows path + snippet and navigates to the
// blob view (no scroll-to-line). Search is project-scoped.
export interface SearchMatch {
  path: string
  snippet?: string
}

// One live OAuth/MCP connection in the OAUTH_LIST response (mirrors Go's
// ui.OAuthConnectionInfo). NO SECRETS: this carries the connecting client's
// identity (client_id = its CIMD metadata URL), the bound principal, the times,
// and the series id (the revoke target + a short prefix for display) — never an
// access/refresh token, code, or PKCE value. Times are RFC3339 strings.
export interface OAuthConnection {
  series_id: string
  series_id_short: string
  client_id: string
  principal_name: string
  principal_email: string
  issued_at: string
  access_expiry: string
}

// The OAUTH_LIST response payload (mirrors Go's ui.OAuthListPayload). The slice
// is always present (possibly empty — the management view's empty state).
export interface OAuthListPayload {
  connections: OAuthConnection[]
}

// The OAUTH_ISSUE_SELF response payload (mirrors Go's ui.OAuthIssueSelfPayload):
// the freshly minted access token for the operator and its expiry. THIS IS THE ONE
// SECRET that crosses /ws/ui — shown once so the operator can copy it into their
// CLI client config; it is never stored client-side beyond the transient display.
export interface OAuthIssueSelfPayload {
  access_token: string
  access_expiry: string
}

// The OAUTH_DENIED frame (mirrors Go's ui.OAuthDeniedPayload): a typed refusal of
// an admin-only OAuth request. reason is "forbidden" (caller is not an
// administrator) or "oauth_disabled" (OAuth is off on this server). Distinct from
// a generic ERROR so the client can recognise an authorization refusal.
export interface OAuthDenied {
  reason: 'forbidden' | 'oauth_disabled' | (string & {})
  message: string
}

// react-arborist node, derived from FileNode via lib/tree.
export interface TreeNode {
  id: string // unique within a project; for files this is the file path
  name: string // leaf label (last path segment)
  path: string // full slash-joined path (directories included)
  isFile: boolean
  children?: TreeNode[]
}
