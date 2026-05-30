---
title: Shoka MCP Interface Contract v1
summary: "Authoritative wire-level contract for Shoka's MCP tools, authentication, webhooks, and error semantics. Quote this; do not re-derive."
status: active
tags: [contract, mcp, interface, shoka]
related:
  - meta/reports/2026-05-27-shoka-verification.md
  - meta/reports/2026-05-28-shoka-schema-fixes-complete.md
  - docs/conventions/frontmatter.md
  - docs/conventions/document-lifecycle.md
  - docs/operations/sensitive-data-removal.md
---

# Shoka MCP Interface Contract v1

This is the single source of truth for Shoka's MCP interface. A client can build
against Shoka using only this document and the reports it cites. Every claim is
sourced (code `file:line`, or a verified report section). Where code and an older
directive disagreed, **the code wins**; such cases are flagged inline.

---

## 1. Versioning and stability

> **Contract status: pre-1.0, may change without notice.** Shoka is in
> dogfooding; backward-compatibility constraints have **not** yet been adopted.
> The file is labelled "v1" for continuity, but the protocol is not yet frozen
> and breaking changes may land between directives. The 2026-05-30 storage
> redesign, for example, renamed `version`→`etag` and `expected_version`→`if_match`
> outright (no alias). Treat the "Stable" list below as the *intent* once 1.0 is
> declared, not a current guarantee.

- This document describes **Shoka MCP v1**, derived from the codebase reachable
  from `master`, updated for the 2026-05-30 storage redesign (file system as
  ground truth, git as a background audit log; `etag`/`if_match` replace
  `version`/`expected_version`).
- **Stable** (changing them is a breaking change requiring a v2 contract):
  - tool names,
  - argument field names and their required/optional status,
  - response field names,
  - webhook event type strings.
- **Non-breaking**: adding a new optional argument field, a new response field, or
  a new tool.
- **Breaking**: removing or renaming any of the stable items, or making an
  optional argument required.

> Note (code vs. history): the post-remediation verification found that all
> "optional" tool fields were in fact *required* at the wire schema (finding F2).
> This was fixed — optional fields now carry `,omitempty` and are genuinely
> optional. This contract describes the **fixed** behavior. See
> `meta/reports/2026-05-28-shoka-schema-fixes-complete.md`.

---

## 2. Transport

- **Protocol:** MCP over **Streamable HTTP**, spec `2025-03-26` (refined
  `2025-06-18`). SSE (the older `2024-11-05` HTTP+SSE transport) has been
  **removed**; there is no SSE endpoint. (Source: `cmd/server/main.go:103-105` —
  `mcp.NewStreamableHTTPHandler(...)`, SDK `go-sdk@v1.6.0/mcp/streamable.go:194`.)
- **Endpoint:** the MCP listener `server.mcp.listen` (e.g. `:8081`). The handler
  is the MCP listener's root handler and is **path-agnostic**, but the canonical,
  documented endpoint path is **`/mcp`**. (Source: `cmd/server/main.go:103-121`.)
  A client registers it with:

  ```
  claude mcp add --transport http shoka http://localhost:<port>/mcp
  ```

- **Single endpoint, three methods** ([spec][st]): the one `/mcp` URL serves
  - **`POST`** — the client sends a JSON-RPC message; the server answers on the
    same response, either as a single `application/json` body or as a
    `text/event-stream` (the SDK default) carrying the response (and, if any, the
    server→client messages tied to that request). The `POST` MUST send
    `Accept: application/json, text/event-stream`.
  - **`GET`** — opens the optional standalone server→client SSE stream
    (`Accept: text/event-stream` required). Shoka pushes no unsolicited
    server→client traffic in normal tool use, so this stream is informational.
  - **`DELETE`** — terminates the session (the SDK client sends this on
    `Close()`).
- **Session identity (`Mcp-Session-Id`):** on the `initialize` response the
  server assigns a session id in the **`Mcp-Session-Id`** response header. The
  client MUST echo that header on **every** subsequent request. (Spec: [§
  Transports][st]; SDK `streamable.go:289`, `streamable.go:1257`.)
- **Stale-session recovery — `404 Not Found`:** if a request presents an
  `Mcp-Session-Id` the server does not recognise — most importantly **after the
  Shoka process restarts**, since sessions are in-memory — the server responds
  **`404 Not Found`** ("session not found"). The SDK client surfaces this as
  `ErrSessionMissing`; the client then re-initializes (a fresh `initialize` with
  no session id) and continues. This is the spec-mandated behaviour and is what
  makes a client survive a server restart. (Spec: *"The server MAY terminate the
  session at any time, after which it MUST respond to requests containing that
  session ID with HTTP 404 Not Found."* — [§ Transports][st]; SDK
  `streamable.go:295-301`, `transport.go:35-37`. Verified live —
  `meta/reports/2026-05-29-shoka-http-transport-complete.md`, the server-restart
  test.)
- **Resumption (`Last-Event-ID`):** the transport supports resuming a dropped
  SSE stream by replaying events after the `Last-Event-ID` the client last saw
  ([spec][st]; SDK `streamable.go:920-934`). Replay requires a server-side
  `EventStore`; Shoka configures **none**, so events are not retained for replay —
  a client that drops its stream simply reconnects (and, if its session is gone,
  re-initializes per the 404 path above).
- **DNS-rebinding protection (Host header):** the SDK's Streamable HTTP handler
  enforces DNS-rebinding protection. When Shoka is reached on a loopback local
  address (`127.0.0.1` / `::1` / `localhost`) but the request's `Host` header is
  **not** a loopback name, the handler returns **`403 Forbidden: invalid Host
  header`** *before* dispatching. The normal registration
  `http://localhost:<port>/mcp` / `http://127.0.0.1:<port>/mcp` never triggers it
  (`localhost` and `127.0.0.1` both count as loopback). It can bite only if a
  non-loopback `Host` (a public hostname, or a reverse proxy's forwarded `Host`)
  reaches a loopback-bound Shoka; in a reverse-proxy deployment, terminate so the
  upstream `Host` is a loopback name, or disable the check by starting Shoka with
  the SDK env var `MCPGODEBUG=disablelocalhostprotection=1`. (Source: SDK
  `go-sdk@v1.6.0/mcp/streamable.go:252-258`.)

[st]: https://modelcontextprotocol.io/specification/2025-03-26/basic/transports

(Source: `cmd/server/main.go:103-121`; SDK `go-sdk@v1.6.0/mcp/streamable.go`;
live verification `meta/reports/2026-05-29-shoka-http-transport-complete.md`.)

---

## 3. Authentication

Controlled by config `server.auth` (Source: `internal/config/config.go:23-30`;
`internal/auth/auth.go`).

- **`server.auth.enabled: false` (default):** no authentication; all requests pass
  and all WebSocket origins are accepted. (Source: `internal/auth/auth.go:39-44`.)
- **`server.auth.enabled: true`:**
  - **The MCP endpoint requires `Authorization: Bearer <token>`** on every
    request. The `?token=` query parameter is **NOT** accepted on the MCP
    endpoint — only on the WebSocket paths `/ws/ui` and `/drafts/`. (Source:
    `internal/auth/auth.go:39-75` header-only `Authenticate`/`Middleware` vs.
    query-allowing `AuthenticateWebSocket`/`MiddlewareAllowQueryToken`; wired at
    `cmd/server/main.go:121` (MCP, header-only) and the WS handlers
    (query-allowing). This was finding **F1**, fixed:
    `meta/reports/2026-05-28-shoka-schema-fixes-complete.md`.)
  - Tokens are compared in **constant time** against the configured set. (Source:
    `internal/auth/auth.go:80-89`.)
  - **Failure response:** HTTP **401** with header `WWW-Authenticate: Bearer` and
    body `unauthorized`. (Source: `internal/auth/auth.go` middleware; verification
    report § 6 Task A.)
  - The credential must be present on **every** request of the session — each
    POST to `/mcp`, the optional GET stream, and the DELETE that ends the session.
    (The Streamable HTTP client must attach the header to all requests.)

---

## 4. Tool catalog

v1 exposes **13 tools**. Twelve are always registered; `translate_file` is
registered **only** when `services.google_cloud.project_id` is set. (Source:
`cmd/server/main.go:131-208`, conditional at `:75` and `:200-205`.)

```
get_server_info  list_projects  create_project  list_files  read_file
read_file_at_version  write_file  delete_file  get_history  read_summary
list_files_since  search_files  translate_file*        (*conditional)
```

### 4.0 Common argument conventions

Apply to every tool unless noted:

- **`namespace`** — optional on every tool; defaults to `"default"` when absent or
  empty. (Source: handlers, e.g. `internal/tools/file.go:36-38`; storage
  `internal/storage/fs_git.go:78-81`.)
- **`namespace` / `project_name` validation** — must match `^[A-Za-z0-9_-]+$`
  (alphanumeric, hyphen, underscore). Invalid names return a tool error. (Source:
  `internal/utils`/`IsValidName`; e.g. `internal/tools/file.go:40-45`.)
- **`if_match`** (write/delete only) — optional; when omitted, the
  optimistic-concurrency check is skipped. When present, the file's current
  `etag` must equal it or the call is rejected with a conflict. See § 5.
- **`path`** — project-relative. Absolute paths and `..` traversal are rejected.
  (Source: `internal/storage/fs_git.go` `relWithin` and parallel checks.)
- **Errors** surface as a **tool-level result with `isError: true`** and a
  human-readable `content[0].text`; they are not JSON-RPC protocol errors. The
  structured errors are the etag conflict (§ 5) and the project-state refusals
  (`reason: "corrupted" | "dangerous" | "write_disabled"`; see § 7). (Source:
  handlers in `internal/tools/*.go`.)

> **ETag values are opaque to clients.** Use an `etag` only as `if_match` on the
> immediately-following `write_file`/`delete_file`. It is the SHA-256 of the
> file's content in the current implementation, but the protocol does not promise
> that and clients must not assume it. It is **not** a git commit hash and is
> **not** valid input to `read_file_at_version` (which takes a `commit_hash` from
> `get_history`).
- **Side effects:** read-only tools produce no commit and no webhook. Mutating
  tools commit and emit a webhook (§ 6). Listed per tool below.

### 4.1 `get_server_info`
- **Purpose:** report the server's configured public URLs and storage location.
- **Input:** none. (Source: `internal/tools/info.go:10-11`.)
- **Output:** `external_url`, `http_listen`, `mcp_listen`, `storage_base_dir`
  (all strings), plus the write-ahead-log status: `wal_pending` (int — entries
  awaiting a background git commit), `wal_write_disabled` (bool — true when the
  WAL has backed up past its threshold and writes are refused), and
  `wal_oldest_entry_age_seconds` (number). These mirror the metrics endpoint
  (§ 7) so operators without Prometheus can observe write-path health over MCP.
  (Source: `internal/tools/info.go`.)
- **Errors:** none expected. **Side effects:** none.

### 4.2 `list_projects`
- **Purpose:** list projects, across all namespaces or scoped to one.
- **Input:** `namespace` (string, **optional**). When omitted, projects from
  **all** namespaces are returned; when given, only that namespace's projects.
- **Output:** `projects` (array of strings; empty array if there are no matching
  projects). Each entry is a `"<namespace>/<name>"` string — the namespace prefix
  is present in **both** modes. Sorted ascending (namespace, then name).
  - `list_projects()` → e.g. `["rohrpost/rohrpost-dev", "shoka/maintenance"]`
  - `list_projects(namespace="shoka")` → e.g. `["shoka/maintenance"]`
- **Side effects:** none. Read-only: no lock, no git access.

  > **Shape note (2026-05-30, B-22 / B-13):** entries are the prefixed
  > `"<namespace>/<name>"` form in both the unscoped and scoped cases — one shape,
  > merely filtered when a namespace is given. This restores the shape B-13
  > originally documented. An omitted `namespace` means **all namespaces**, not
  > the `"default"` namespace. (Source: `internal/tools/project.go`;
  > `internal/storage/namespace.go`.)

### 4.3 `create_project`
- **Purpose:** create a project and initialize its Git repository.
- **Input:** `namespace` (optional, default `"default"`), `project_name` (string,
  **required**).
- **Output:** `message` (string).
- **Side effects:** `git init` the project directory (no commit is produced);
  emits a **`project_created`** webhook (no `commit_hash`). Creating an existing
  project is a no-op success. (Source: `internal/tools/project.go:15-53`;
  `internal/storage/fs_git.go:91-118`.)

### 4.4 `list_files`
- **Purpose:** list files (non-recursive) in a project path, optionally with
  summaries.
- **Input:** `namespace` (opt), `project_name` (**required**), `path` (opt,
  default project root), `include_summaries` (bool, opt, default `false`).
- **Output:** `files` (array; directories carry a trailing `/`); `summaries`
  (object: file → `{frontmatter, heading, etag}`) present only when
  `include_summaries`. `.git`, `.drafts`, and `.shoka` are never listed. **No
  status filtering** — every file is returned regardless of frontmatter `status`.
  (Source: `internal/tools/project.go`.)
  > Removed in the storage redesign: the `include_versions` argument and the
  > `versions` output map. Callers that want an etag pass `include_summaries`
  > (each summary carries `etag`) or call `read_file`.
- **Side effects:** none.

### 4.5 `read_file`
- **Purpose:** read a file's full content plus its current etag.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**).
- **Output:** `content` (string), `etag` (string — opaque; SHA-256 of the
  content). Pass `etag` as `if_match` on a later write/delete. (Source:
  `internal/tools/file.go`; `internal/storage/fs_git.go` `ReadFileWithETag`.)
- **Errors:** missing file, or project in `dangerous` state → `isError: true`.
  **Side effects:** none.

### 4.6 `read_file_at_version`
- **Purpose:** read a file's content as of a specific commit (e.g. to recover a
  deleted or superseded version). This is the "past access" API and is git-backed.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `commit_hash` (string, **required** — a **git commit hash** from
  `get_history`; *not* an `etag`).
- **Output:** `content` (string).
- **Errors:** unknown hash or path-not-in-that-commit → `isError: true`.
  **Side effects:** none. (Source: `internal/tools/file.go`;
  `internal/storage/fs_git.go` `ReadFileAtVersion`.)

### 4.7 `write_file`
- **Purpose:** create or overwrite a file. The file system is updated
  synchronously; the git commit is performed in the background (§ 7).
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `content` (string, **required**), `if_match` (string, optional
  — the etag the file is expected to be at; omit to skip the check; see § 5).
- **Output (success):** `message` (string), `etag` (string — the new content
  etag).
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_etag: <etag>` (see § 5).
- **Output (project-state refusal):** `isError: true`; structured
  `reason: "corrupted" | "dangerous" | "write_disabled"` (no etag; see § 7).
- **Side effects:** synchronous atomic file write + WAL append; a background
  git commit (`Update <path>`) and a **`file_written`** webhook with `commit_hash`
  follow asynchronously. (Source: `internal/tools/file.go`;
  `internal/storage/fs_git.go` write path + `commit.go`.)

### 4.8 `delete_file`
- **Purpose:** remove a file from the current tree; the deletion is committed in
  the background. History is preserved — content remains recoverable (§ 7).
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `if_match` (string, optional — omit to skip the check).
- **Output (success):** `message` (no etag — the file is gone).
- **Output (conflict):** `isError: true`; `conflict: true`, `current_etag`
  (see § 5).
- **Output (project-state refusal):** `isError: true`; `reason` (see § 7).
- **Side effects:** synchronous file removal + WAL append; a background commit
  (`Delete <path>`) and a **`file_deleted`** webhook with `commit_hash` follow.
  (Source: `internal/tools/file.go`; `internal/storage/fs_git.go`.)

### 4.9 `get_history`
- **Purpose:** commit history for a project or a single file.
- **Input:** `namespace` (opt), `project_name` (**required**), `path` (opt — empty
  = whole-project history), `limit` (int, opt, default `10`), `since` (string,
  opt — an RFC3339 timestamp or a commit hash; **exclusive**).
- **Output:** `history` — array of `{hash, author, date, message}` (`date` is
  RFC3339). When `since` is set, results are filtered to commits after that point
  then truncated to `limit`. (Source: `internal/tools/file.go:172-253`;
  `internal/storage/storage.go:6-11` (`CommitInfo`); `internal/storage/fs_git.go:376-428`.)
- **Side effects:** none.

### 4.10 `read_summary`
- **Purpose:** context-efficient view of a Markdown file — **never returns the
  full body**.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**).
- **Output:** `frontmatter` (object; empty if absent/malformed), `heading`
  (first heading), `excerpt` (first paragraph, capped at **200 runes**), `size`
  (int, bytes), `etag` (opaque content SHA-256), `modified_at` (RFC3339 of the
  last commit, omitted until the background commit lands). (Source:
  `internal/tools/summary.go`; `docs/conventions/frontmatter.md`.)
- **Side effects:** none.

### 4.11 `list_files_since`
- **Purpose:** list files changed after a point in time/history, with the kind of
  change.
- **Input:** `namespace` (opt), `project_name` (**required**), `since` (string,
  **required** — RFC3339 timestamp or commit hash, exclusive).
- **Output:** `changes` — array of `{path, hash, kind}` where `kind` is one of
  `added`, `modified`, `deleted`. (Source: `internal/tools/discovery.go:12-50`;
  `internal/storage/discovery.go:19-23`.)
- **Side effects:** none.

### 4.12 `search_files`
- **Purpose:** case-insensitive substring search over filenames and/or content.
  Reaches retired/`failed` documents (no status filtering) — see
  `docs/conventions/failure-records.md`.
- **Input:** `namespace` (opt), `project_name` (**required**), `query` (string,
  **required**), `search_in` (string, opt — `filename` | `content` | `both`;
  default `both`).
- **Output:** `matches` — array of `{path, snippet}` (`snippet` omitted for
  filename-only matches; content snippets are bounded, ≤100 runes/side). (Source:
  `internal/tools/discovery.go:52-91`; `internal/storage/discovery.go:26-31,146-205`.)
- **Side effects:** none.

### 4.13 `translate_file` (conditional)
- **Availability:** registered **only** when `services.google_cloud.project_id`
  is configured. (Source: `cmd/server/main.go:75-83,200-205`.)
- **Purpose:** translate a Markdown file and write the result as a sibling file.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `target_lang` (string, opt, default `"en"`).
- **Output:** `output_path` (string — `<base>.<target_lang><ext>`), `message`.
- **Side effects:** reads the source, calls Google Cloud Translation, then
  `write_file`-equivalent commit of the output → emits a **`file_written`**
  webhook for the output path. (Source: `internal/tools/translation.go:15-86`.)

---

## 5. Optimistic concurrency (ETag / if_match)

(Source: `internal/storage/fs_git.go` write/delete paths and `VersionConflictError`;
`internal/tools/file.go`.)

- A file's **etag** is an opaque token (the SHA-256 of its content; `""`-equivalent
  for an absent file is the empty-content hash). It is returned by `read_file`
  (and in `read_summary`/`list_files` summaries), and is **not** a git commit hash.
- **To enforce a check:** pass `if_match` (an etag from `read_file` or a prior
  write) to `write_file`/`delete_file`. If it does not equal the file's current
  etag, the call is rejected with a conflict and **no change is made**.
- **To skip the check:** omit `if_match` (a blind write).
- **Conflict response shape:**

  ```json
  {
    "content": [
      { "type": "text", "text": "etag conflict: file is now at <current_etag> (you sent if_match <your_etag>); re-read the file and retry with the current etag" }
    ],
    "structuredContent": {
      "conflict": true,
      "current_etag": "<current_etag>"
    },
    "isError": true
  }
  ```

  - It is a **tool-level error** (`isError: true`), **not** a JSON-RPC error.
- **Recovery procedure:** on conflict, call `read_file` to get the current
  `etag`, reconcile your change against the current content, then retry the write
  with the new `if_match`.

---

## 6. Webhooks

Configured via `webhooks` in config (Source: `internal/config/config.go:32-39,55`).
Emitted at the storage layer, so **every** write path (MCP tools and the web UI)
triggers them. (Source: `internal/storage/fs_git.go:27-51,110-116,194-201,325-332`;
`cmd/server/main.go:47-57`.)

- **HTTP method/target:** `POST` to each subscribed `url`.
- **Headers** (verification report § 2.4):
  - `Content-Type: application/json`
  - `X-Shoka-Signature: <lowercase hex HMAC-SHA256>` — present **only** when the
    hook has a `secret`.
  - Go HTTP client defaults: `User-Agent: Go-http-client/1.1`,
    `Accept-Encoding: gzip`, `Content-Length`.
- **Body schemas** (verification report § 2.4; Source:
  `internal/webhooks/webhooks.go:26-33`):
  - `file_written` / `file_deleted`:
    ```json
    {"event":"file_written","namespace":"…","project":"…","path":"…","commit_hash":"…","timestamp":"<RFC3339Nano>"}
    ```
  - `project_created` (**no `path`, no `commit_hash`**):
    ```json
    {"event":"project_created","namespace":"…","project":"…","timestamp":"<RFC3339Nano>"}
    ```
- **Event types (exactly three):** `file_written`, `file_deleted`,
  `project_created`. A hook receives only the events listed in its `events`.
- **HMAC recipe** (verification report § 2.5): `X-Shoka-Signature =
  lowercasehex(HMAC_SHA256(key=<hook secret>, msg=<exact raw request body bytes>))`.
  No canonicalization — sign/verify the exact bytes. Shell:
  `printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex`.
- **Delivery semantics:** asynchronous and best-effort; the originating MCP call
  never blocks on or fails because of delivery. **2 attempts**, **200 ms** backoff
  (doubling), failures are logged and never propagated. In-flight deliveries are
  drained on graceful shutdown. (Source: `internal/webhooks/webhooks.go:62-123`;
  verification report § 6 Task E.)

---

## 7. Storage model

(Source: `internal/storage/`; design log `meta/design/2026-05-30-storage-redesign.md`.)

**The file system is the ground truth; git is a background audit log** (2026-05-30
storage redesign).

- Files live at **`<base_dir>/<namespace>/<project>/<path>`**. Each project is its
  own Git repository (`git.PlainInit`).
- **Reads** (`read_file`, `list_files`, `read_summary`, `search_files`) are served
  straight from the working tree — no lock, no git on the path.
- **Writes/deletes** update the working tree atomically and append to a
  **write-ahead log** at `<base_dir>/.shoka/wal/`, then return. A background worker
  pool commits each WAL entry to git asynchronously (`Update <path>` /
  `Delete <path>`, author `MCP Server <mcp-server@shoka.io>`), one commit per WAL
  entry. So `get_history`/`read_file_at_version`/`list_files_since` reflect a write
  only after its commit lands (typically within milliseconds; observable via
  `get_server_info`'s `wal_pending`).
- **Write-disabled mode:** if the WAL backs up past its configured threshold,
  `write_file`/`delete_file` are refused with `reason: "write_disabled"` until it
  drains. Reads are unaffected.
- **Per-project state:** a project is `healthy`, `corrupted` (working tree drifted
  from git HEAD outside the write path), or `dangerous` (`.git` unreadable).
  `corrupted`/`dangerous` projects refuse writes (`reason: "corrupted" |
  "dangerous"`); `dangerous` also refuses reads. Recovery is an operator action
  (Web UI or the `shoka project recover` CLI), not an MCP tool.
- **History is preserved indefinitely.** Past content of deleted or overwritten
  files is retrievable with `read_file_at_version` against a commit from
  `get_history`.
- **Metrics:** an optional Prometheus endpoint (off by default, loopback-only;
  config `metrics.addr`) exposes WAL depth, write-disabled state, commit
  counts, file-lock leases, and per-project state. The pprof endpoint
  (`--profile-addr`) follows the same defaults.

---

## 8. What this contract does NOT cover

- **History rewriting** (e.g. `git filter-repo`) is **not** exposed through MCP. It
  is an operator procedure — see `docs/operations/sensitive-data-removal.md`.
- **MCP resource subscriptions / server push** are not implemented. Use **webhooks**
  for change notification. (No resources/subscriptions are registered in
  `cmd/server/main.go`.)
- **Diffing arbitrary commits** is not a tool. Fetch two versions with
  `read_file_at_version` and diff client-side.
- **No multi-tenancy beyond namespaces**, and **no per-namespace ACLs**.
  Authentication is a single shared Bearer-token set (§ 3); it does not scope
  access per namespace.

---

## Sources

- **Reports:** `meta/reports/2026-05-27-shoka-verification.md` (§ 2.1 transport,
  § 2.2 conflict shape, § 2.4 webhook shape, § 2.5 HMAC, § 6 raw observations);
  `meta/reports/2026-05-28-shoka-schema-fixes-complete.md` (F1 auth scope, F2
  optional fields).
- **Source files:** `cmd/server/main.go:75-216` (transport, tool registration,
  auth/webhook wiring); `internal/auth/auth.go:39-89`; `internal/config/config.go:11-56`;
  `internal/storage/fs_git.go` (storage, locking, commits, events);
  `internal/storage/storage.go:6-11` (`CommitInfo`);
  `internal/storage/discovery.go:19-31` (`FileChange`, `SearchMatch`);
  `internal/webhooks/webhooks.go:26-123`; `internal/tools/*.go` (per-tool schemas).
- **Conventions:** `docs/conventions/frontmatter.md`,
  `docs/conventions/document-lifecycle.md`, `docs/conventions/failure-records.md`.

---

## Operational notes (non-normative)

The server writes structured diagnostic logs to **stderr**. This adds **no
wire-visible behavior**: no request or response field is added, removed, or
changed. Logs never contain file content or auth tokens — metadata only (method
names, session IDs, outcome labels). See `docs/OPERATIONS.md` § Logging for
configuration details (`server.log.level`, `server.log.format`).
- **Diagnostic logging (non-normative).** When the server is run with
  `server.log.level: debug`, it emits redacted protocol-level traces (JSON-RPC
  request/response bodies, the assigned `Mcp-Session-Id`, and server→client
  stream frames) to stderr. This is operational instrumentation only: it does not
  change the wire protocol, message shapes, or any behavior described above, and
  never logs file contents or credentials.
