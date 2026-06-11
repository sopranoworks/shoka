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
  repository at `<base_dir>/<namespace>/<project>`. A write or delete is an atomic
  working-tree update plus an append to a write-ahead log (WAL) on the request path,
  taken under a per-file lock (reads take no lock); a background worker pool drains
  the WAL and commits to Git asynchronously. The working tree is ground truth and
  the Git history is the audit trail. No database. (Source:
  `internal/storage/fs_git.go`, `internal/storage/wal`,
  `internal/storage/walworker`.)
- **Catalog (project index)** — `internal/storage`, a bbolt database beside each
  project (`<base_dir>/<namespace>/<project>.db`). A rebuildable cache of the file
  set and each file's `etag`, so listings and lookups need not walk the working
  tree. (Source: `internal/storage/catalog_store.go`.)
- **Search index** — `internal/storage`, a bbolt database beside each project
  (`<base_dir>/<namespace>/<project>.index.db`). A rebuildable full-text (bigram)
  and reverse-link index maintained by a background sweep, backing `search_files`
  and link integrity. (Source: `internal/storage/index_store.go`,
  `internal/storage/index_sweep.go`.)
- **Lost+found worker** — `internal/storage`. A periodic sweep over each namespace:
  untracked files matching the `shoka.disposable` marker are deleted, anything else
  is relocated to a `.shoka-lostfound` holding area rather than altered in place, so
  project trees stay clean without losing data. (Source:
  `internal/storage/lostfound_area.go`.)
- **MCP server (Streamable HTTP)** — `internal/tools` + the MCP Go SDK. Exposes
  the 18 tools (path `/mcp`) over the Streamable HTTP transport (spec
  `2025-03-26`) on up to two listeners selected by config presence: the plain
  `server.mcp.plain.listen` (unauthenticated, or static-bearer when
  `bearer_auth`) and the OAuth-protected `server.mcp.oauth.listen` — one shared
  MCP server behind per-port authenticators. (Source: `cmd/server/main.go`,
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
- **Auth (Bearer + OAuth 2.1 AS)** — `internal/auth`, `internal/oauth`. Optional
  Bearer-token authentication and WebSocket origin policy, disabled by default;
  header-only on the MCP endpoint, with a `?token=` query fallback allowed only on
  the WebSocket paths. Shoka also embeds a built-in OAuth 2.1 authorization server
  — metadata discovery, `/authorize` (consent + PKCE), and `/token` — which, when
  enabled, governs access to the MCP path in place of the static Bearer set.
  Described here by role only; enabling it is a deployment concern (see
  `docs/OPERATIONS.md`). (Source: `internal/auth/auth.go`, `internal/oauth`.)
- **Metrics** — `internal/metrics`. An optional Prometheus `/metrics` endpoint,
  loopback-only and disabled by default, exposing operational counters and gauges
  across the storage, WAL, index, lost+found, catalog, and OAuth subsystems.
  (Source: `internal/metrics`.)

## Design choices and why

- **Git as the backend (no separate database).** History preservation and version
  identity come for free from Git. Optimistic locking, however, does **not** use the
  commit hash: a file's lock token is its `etag` — the SHA-256 of its current
  content — and `write_file` accepts it as `if_match`. The Git commit hash is a
  separate identifier, used only by `read_file_at_version` and `get_history` to
  address a point in history. (Source: `internal/storage/fs_git.go`.)
- **Write-ahead log with asynchronous commit.** The request path does the minimum
  for durability and correctness — an atomic working-tree write plus a WAL append
  under a per-file lock — then returns; a background worker pool commits to Git off
  the critical path. Writes stay fast, and per-repository Git contention does not
  block callers. The working tree, not the latest commit, is ground truth. (Source:
  `internal/storage/wal`, `internal/storage/walworker`.)
- **Derived state is disposable.** The catalog and the search index are rebuildable
  caches kept beside each project, never the source of truth; a missing or stale one
  is rebuilt from the working tree. Untracked clutter is swept to a lost+found area
  (or deleted when marked `shoka.disposable`) rather than committed. (Source:
  `internal/storage/catalog_store.go`, `internal/storage/index_sweep.go`,
  `internal/storage/lostfound_area.go`.)
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
  `internal/storage/fs_git.go` (storage/versioning), `internal/storage/wal` +
  `internal/storage/walworker` (WAL + async commit),
  `internal/storage/catalog_store.go` (catalog), `internal/storage/index_sweep.go`
  (search index), `internal/storage/lostfound_area.go` (lost+found),
  `internal/webhooks/webhooks.go` (notifier), `internal/auth/auth.go` +
  `internal/oauth` (auth), `internal/metrics` (metrics),
  `internal/drafts/manager.go` (drafts).
- Documents: `docs/contracts/mcp-v1.md`, `REQUIREMENTS.md`,
  `docs/operations/sensitive-data-removal.md`.
