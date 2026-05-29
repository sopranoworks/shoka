# Shoka Architecture

How Shoka is built and why. For the wire interface see
[`docs/contracts/mcp-v1.md`](contracts/mcp-v1.md); for running it see
[`docs/OPERATIONS.md`](OPERATIONS.md).

## Why Shoka exists

Coding agents need authoritative, versioned instructions and documentation that
live *outside* the project repository they describe — so the knowledge base can be
edited by humans, translated, and audited independently of any one codebase. Shoka
is that store: human-authored Markdown in, Git-versioned history kept, agent-
readable documents out over MCP.

## Components

Shoka is a single Go binary running two HTTP listeners (Source:
`cmd/server/main.go:95-116`).

- **Storage (Git-backed filesystem)** — `internal/storage`. Each project is a Git
  repository at `<base_dir>/<namespace>/<project>`. Writes and deletes are atomic
  commits; history is the audit trail. No database. (Source:
  `internal/storage/fs_git.go`.)
- **MCP server (Streamable HTTP)** — `internal/tools` + the MCP Go SDK. Exposes
  the 13 tools on the `server.mcp.listen` port (path `/mcp`) over the Streamable
  HTTP transport (spec `2025-03-26`). (Source: `cmd/server/main.go:103-121`,
  `setupMCPServer`.)
- **Web UI (WebSocket + draft persistence)** — `internal/ui`, `internal/drafts`,
  plus an embedded React build served on the `server.http.listen` port. Drafts are
  persisted over a WebSocket (`/drafts/{namespace}/{project}`) and replayed on
  reconnect so unstable/mobile clients do not lose work. (Source:
  `cmd/server/main.go:210-239`; `internal/drafts/manager.go`.)
- **Webhook notifier** — `internal/webhooks`. Registered as the storage change
  handler, so *every* write path (MCP and web UI) emits `file_written`,
  `file_deleted`, or `project_created` to subscribed URLs — asynchronously,
  best-effort, signed with HMAC-SHA256 when a secret is set. (Source:
  `internal/storage/fs_git.go:41-51`; `cmd/server/main.go:47-57`;
  `internal/webhooks/webhooks.go`.)
- **Auth middleware** — `internal/auth`. Optional Bearer-token authentication and
  WebSocket origin policy; disabled by default. Header-only on the MCP endpoint;
  `?token=` query fallback allowed only on the WebSocket paths. (Source:
  `internal/auth/auth.go`; `cmd/server/main.go:104,215-216`.)

## Design choices and why

- **Git as the backend (no separate database).** History preservation, atomic
  commits, and version identity all come for free from Git. A document's "version"
  is just its latest commit hash — which is also the optimistic-locking token.
  (Source: `internal/storage/fs_git.go:473-516`.)
- **Namespace/project filesystem isolation (no IDs, no metadata service).**
  Project identity is the human-readable `namespace/project` path. There is no
  UUID and no mapping table to keep consistent; isolation is enforced by name
  validation (`[A-Za-z0-9_-]+`) plus path-traversal guards. (Source:
  `internal/storage/fs_git.go:78-89,142-145`; `REQUIREMENTS.md` META-01/02.)
- **`delete_file` as `git rm` (forward-only history).** Deletes remove a file from
  the current tree but never rewrite history, so deleted content stays recoverable
  via `read_file_at_version`. History *rewriting* is deliberately not an MCP
  capability. (Source: `internal/storage/fs_git.go:256-335`;
  `docs/operations/sensitive-data-removal.md`.)
- **Webhooks instead of MCP push/subscriptions.** Clients may be ephemeral (a
  window that comes and goes, a Cloud Run worker that scales to zero), so Shoka
  pushes change notifications outward via HTTP rather than relying on a persistent
  MCP subscription. Delivery never blocks or fails the originating write. (Source:
  `internal/webhooks/webhooks.go:62-123`.)

## Topology

One supported deployment topology (Shoka itself is client-agnostic):

```
iOS Claude app → Remote Control → `claude code` (a per-project window) → Shoka MCP → Cloud Run workers
```

Clients do not connect to Shoka directly; a `claude code` instance acts as the
window and is the MCP client. The only network surfaces are the MCP (Streamable
HTTP) endpoint and the web UI's WebSocket endpoints — there is no separate mobile
REST API.

## Sources

- Source: `cmd/server/main.go` (composition, listeners, wiring),
  `internal/storage/fs_git.go` (storage/versioning), `internal/webhooks/webhooks.go`
  (notifier), `internal/auth/auth.go` (auth), `internal/drafts/manager.go` (drafts).
- Documents: `docs/contracts/mcp-v1.md`, `REQUIREMENTS.md`,
  `docs/operations/sensitive-data-removal.md`.
