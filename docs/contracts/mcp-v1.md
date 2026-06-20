---
title: Shoka MCP Interface Contract v1
summary: "Authoritative wire-level contract for Shoka's MCP tools, authentication, webhooks, and error semantics. Quote this; do not re-derive."
status: active
tags: [contract, mcp, interface, shoka]
related:
  - docs/conventions/frontmatter.md
  - docs/conventions/document-lifecycle.md
  - docs/operations/sensitive-data-removal.md
---

# Shoka MCP Interface Contract v1

This is the single source of truth for Shoka's MCP interface. A client can build
against Shoka using only this document. Every claim is sourced to code
(`file:line`). Where code and an older directive disagreed, **the code wins**;
such cases are flagged inline.

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
> optional. This contract describes the **fixed** behavior.

---

## 2. Transport

- **Protocol:** MCP over **Streamable HTTP**, spec `2025-03-26` (refined
  `2025-06-18`). SSE (the older `2024-11-05` HTTP+SSE transport) has been
  **removed**; there is no SSE endpoint. (Source: `cmd/shoka/main.go:103-105` —
  `mcp.NewStreamableHTTPHandler(...)`, SDK `go-sdk@v1.6.0/mcp/streamable.go:194`.)
- **Endpoint:** Shoka opens up to **two** MCP listeners, selected by config
  presence: the **plain** transport `server.mcp.plain.listen` and the **OAuth**
  transport `server.mcp.oauth.listen` (at least one must be set; see § 3). Each
  serves the same MCP server over its own handler. The handler is the listener's
  root handler and is **path-agnostic**, but the canonical, documented endpoint
  path on either port is **`/mcp`**. (Source: `cmd/shoka/main.go` MCP transport
  wiring; `internal/config/config.go:84-127`.) A client registers it with:

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
  `streamable.go:295-301`, `transport.go:35-37`.)
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

(Source: `cmd/shoka/main.go:103-121`; SDK `go-sdk@v1.6.0/mcp/streamable.go`.)

---

## 3. Authentication

**MCP authentication is decided per transport — by which port a request arrives
on**, not by a single global switch. The two MCP transports (§ 2) have distinct,
non-mixable auth models (Source: `internal/config/config.go:84-127`;
`cmd/shoka/main.go` per-port authenticator construction; `internal/auth/auth.go`):

- **Plain transport (`server.mcp.plain`)** — the normal/internal port:
  - **`bearer_auth: false` (default):** no authentication; every request reaches
    the MCP handler. Intended for loopback/internal use behind the network
    boundary. (Source: `internal/config/config.go:95-99`; `internal/auth/auth.go`
    disabled authenticator.)
  - **`bearer_auth: true`:** the port requires `Authorization: Bearer <token>` on
    every request, validated in **constant time** against the **`server.auth.tokens`**
    set. The `?token=` query parameter is **NOT** accepted on the MCP endpoint —
    only on the WebSocket paths `/ws/ui` and `/drafts/`. Failure is HTTP **401**
    with `WWW-Authenticate: Bearer` and body `unauthorized`. The credential must
    be present on **every** request of the session (each POST to `/mcp`, the
    optional GET stream, and the DELETE that ends it). (Source:
    `internal/auth/auth.go` header-only `Middleware` vs. query-allowing
    `MiddlewareAllowQueryToken`; this was finding **F1**, fixed.)
  - The plain port is **never** OAuth: no discovery/AS surface, no token-store
    enforcement.
- **OAuth transport (`server.mcp.oauth`)** — the external port: **pure OAuth**
  (§ 3.1). It requires a valid OAuth access token on every MCP request and never
  accepts a static `server.auth.tokens` bearer.

`server.auth` is therefore **no longer a global MCP gate**: `server.auth.tokens`
is the static-bearer source the plain port validates against when its `bearer_auth`
is on, and `server.auth.{enabled,allowed_origins}` govern the Web UI / non-MCP
endpoints (`/ws/ui`, `/drafts`, `/api`) — token + WebSocket-origin policy.
(Source: `internal/config/config.go:57-70`.)

### 3.1 OAuth 2.1 authorization server (the OAuth transport)

> **Active when the OAuth transport is configured.** OAuth is in force exactly
> when `server.mcp.oauth.listen` is set (there is no separate enable flag); it
> applies to the **OAuth port only**. This subsection describes what that port
> does and the configuration an operator must supply — it names config fields and
> protocol roles only, never an address, endpoint URL, token, or secret. When no
> OAuth transport is configured, only the plain-port rules of § 3 apply.

When enabled, Shoka acts as **its own built-in OAuth 2.1 authorization server**
protecting the MCP resource, so a client can obtain a bearer access token instead
of being handed a static one.

- **Flow:** OAuth 2.1 **authorization-code** with **PKCE (`S256`) mandatory**;
  the `/token` exchange returns an access token and a **rotating** refresh token,
  and binds the audience to this server's MCP resource per RFC 8707. (Source:
  `internal/oauth/server.go:134-227,338-408,433-439`.)
- **Discovery — by role, not by address:** the server publishes an RFC 9728
  **Protected Resource Metadata** document and an RFC 8414 **Authorization Server
  Metadata** document; the latter advertises the authorization and token endpoints
  and declares `response_types_supported: ["code"]`,
  `code_challenge_methods_supported: ["S256"]`, and
  `client_id_metadata_document_supported: true`. When a request is unauthorized,
  the `401`'s `WWW-Authenticate: Bearer` challenge carries the **`resource_metadata`**
  parameter (RFC 9728 § 5.1) pointing the client at the metadata document. A client
  discovers every endpoint from these documents — **the contract names the documents
  by role and never a literal URL.** (Source: `internal/oauth/discovery.go:27-97`;
  `internal/auth/auth.go:43-49,141-152`.)
- **Client identity — Client ID Metadata Document (CIMD), not dynamic registration:**
  there is **no dynamic client registration endpoint** (`registration_endpoint` is
  deliberately omitted — do not look for one). Instead the client's `client_id`
  **is an https URL to its own client-metadata document**; the server fetches that
  document and only trusts it when its domain is a trusted **"domain" entry in the
  dynamic domain store**, managed by the operator in the **web UI** (Settings → OAuth) —
  **default-deny** (no entries ⇒ no client may connect). Trust is no longer a config
  field (B-71 Stage 2e); the store is the sole runtime source. (Source:
  `internal/oauth/cimd.go` `Verifier.SetTrustedSource`; `internal/storage/oauthstore`
  `TrustedDomain`; `internal/ui/manager.go` domain ops.)
- **Consent:** approval at `/authorize` is **per-domain**: the submitted credential is
  verified (hashed, constant-time) against the connecting client's domain's own stored
  consent, set by the operator in the web UI. A domain with **no per-domain consent**,
  or a client with **no domain entry** (and not a confidential client, below),
  **cannot be approved** — there is **no global consent fallback** (B-71 Stage 2e retired
  the `consent_credential` config key). The consent value is stored hashed and never
  returned. (Source: `internal/oauth/server.go` `authorizeConsent`;
  `internal/storage/oauthstore` `VerifyConsent`.)
- **Confidential pre-issued clients (Client ID + Secret) — B-71 Stage 3:** the operator
  may **pre-issue a Client ID + Secret** from the web UI (super-user only) for a Claude.ai
  custom connector. The secret is **shown once** and stored **hashed** (never retrievable);
  the credential carries a **scope** (enforced by the tools/call namespace gate) and a
  **finite expiry** (no indefinite). Such a client is **pre-authorized by issuance**, so
  `/authorize` grants consent on approval **without a consent credential** — the gate is the
  **secret at `/token`**, verified (`client_secret_basic` / `client_secret_post`,
  constant-time) **in addition to the mandatory PKCE**. `token_endpoint_auth_methods_supported`
  advertises these methods alongside `none`. Confidential auth is **additive** — the public
  CIMD/DCR/self (PKCE-only) path is unchanged. (Source: `internal/oauth/server.go`
  `grantAuthorizationCode`; `internal/storage/oauthstore` confidential entry;
  `internal/ui/manager.go` `CLIENT_*` ops.)
- **Enablement prerequisites (operator-supplied, names only):** the OAuth
  transport activates by **setting `server.mcp.oauth.listen`** (there is no
  separate enable flag — presence is the switch). A production deployment also
  requires a **TLS-terminating reverse proxy** in front of Shoka (Shoka
  terminates no TLS itself), **`server.mcp.oauth.external_url`** set to the public
  URL (the field name — the URL value lives only in config), and **at least one
  trusted domain added in the web UI with its consent secret set** (Settings → OAuth) —
  these live in the dynamic domain store, not config. Global default token lifetimes are
  `server.mcp.oauth.access_token_ttl` / `refresh_token_ttl` / `authorization_code_ttl`
  (per-domain TTL overrides are set in the UI). The former
  `trusted_client_metadata_domains` / `consent_credential` keys are **deprecated**: they
  parse but are consumed once to migrate a not-yet-seeded deployment, then ignored.
  (Source: `internal/config/config.go`; `cmd/shoka/main.go` OAuth wiring.)
- **What an MCP client does (by port):**
  - **On the OAuth port** — discover the AS from the metadata documents (above), run
    the standard authorization-code + PKCE-`S256` handshake to obtain an access token,
    then present it as `Authorization: Bearer <access token>` on every MCP request.
    A request without a valid OAuth access token — including one carrying a static
    `server.auth.tokens` bearer — is refused with `401` and the `resource_metadata`
    challenge. (Source: `internal/auth/auth.go:109-122`; per-port authenticators in
    `cmd/shoka/main.go`.)
  - **On the plain port** — no OAuth: `bearer_auth: false` ⇒ no auth;
    `bearer_auth: true` ⇒ `Authorization: Bearer <static token>` (validated against
    `server.auth.tokens`) on every request, per § 3. The OAuth token check is never
    installed on this port. (Source: `internal/auth/auth.go:85-122`;
    `internal/config/config.go:95-99`.)

---

## 4. Tool catalog

v1 exposes **18 tools**, all always registered. (Source: `cmd/shoka/main.go`.)
The former conditional `translate_file` tool was **retired** (see §4.17).

```
get_server_info  list_projects  create_project  list_files  read_file
read_file_at_version  write_file  delete_file  append_to_file  patch_file
move_file  get_history  read_summary  list_files_since  search_files
recover_project  subscribe  unsubscribe
```

`subscribe` / `unsubscribe` register scoped file-change notifications delivered as
`notifications/message` (the B-45b subscription tools).

### 4.0 Common argument conventions

Apply to every tool unless noted:

- **`namespace`** — optional on every tool; defaults to `"default"` when absent or
  empty. (Source: handlers, e.g. `internal/tools/file.go:36-38`; storage
  `internal/storage/fs_git.go:78-81`.)
- **`namespace` / `project_name` validation** — must match `^[A-Za-z0-9_-]+$`
  (alphanumeric, hyphen, underscore). Invalid names return a tool error. (Source:
  `internal/utils`/`IsValidName`; e.g. `internal/tools/file.go:40-45`.)
- **`if_match`** (`write_file`/`delete_file`/`append_to_file`/`patch_file`/
  `move_file`) — optional; when omitted, the optimistic-concurrency check is
  skipped. When present, the file's current `etag` must equal it or the call is
  rejected with a conflict. See § 5. It is the **only** integrity argument: there
  is deliberately no `sha256` argument on the partial-edit tools (see § 4.14/4.15).
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
- **Output:** `files` (array; directories carry a trailing `/`); `modified_at`
  (object: entry → RFC3339 timestamp; **always present**, keyed by every entry in
  `files`, directories included); `summaries` (object: file →
  `{frontmatter, heading, etag, modified_at}`) present only when
  `include_summaries`. `.git`, `.drafts`, and `.shoka` are never listed. **No
  status filtering** — every file is returned regardless of frontmatter `status`.
  (Source: `internal/tools/project.go`.)
  > `modified_at` is the **file-system mtime** (`os.Stat().ModTime()`) in RFC3339
  > **nanosecond** precision, UTC. It reflects the **working tree**, not the git
  > audit log, so it is available immediately after a write. It shares its source,
  > format, and timing with `read_summary`'s `modified_at` **by design** — for any
  > path that exists, `read_summary.modified_at`, `list_files.modified_at[<path>]`,
  > and (when `include_summaries` is set) `summaries[<path>].modified_at` return the
  > **same string** at the same moment. (`get_history` still returns git commit
  > times, which is the correct semantic for a history listing.) Directory mtimes
  > are included for consistency but typically change only when entries are added or
  > removed, not when a child's contents change.
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
- **Purpose:** create a new file **or overwrite an existing one** — writing to a
  path that already exists replaces its whole content (no separate `delete_file`
  is needed). The overwrite is safe because git is the backstop: every write is an
  atomic commit, so the prior content stays recoverable via `get_history` /
  `read_file_at_version`. `if_match` is the **optional** optimistic-concurrency
  guard, not an existing-path gate — omit it and the overwrite always proceeds;
  supply the file's current etag and the write is rejected with a conflict if the
  file changed since (§ 5). The file system is updated synchronously; the git
  commit is performed in the background (§ 7).
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `content` (string, **required**), `if_match` (string, optional
  — the etag the file is expected to be at; omit to skip the check; see § 5),
  `content_encoding` (string, optional — `"utf8"` (default, or omitted) treats
  `content` as literal text; `"base64"` decodes `content` from standard base64 to
  raw bytes before writing, for byte-faithful ingest of an existing file whose
  bytes may not be valid UTF-8). The decode happens server-side, before the
  ordinary guarded write path — there is no separate ingest path.
- **Output (success):** `message` (string), `etag` (string — the new content
  etag).
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_etag: <etag>` (see § 5).
- **Output (project-state refusal):** `isError: true`; structured
  `reason: "corrupted" | "dangerous" | "write_disabled"` (no etag; see § 7).
- **Output (ingest refusal, `content_encoding: "base64"` only):** `isError: true`;
  structured `reason: "format_rejected"` when `path` is outside the ingest
  allowlist — the closed set `.md`/`.markdown`/`.json`/`.yaml`/`.yml`
  (case-insensitive; an extensionless path is rejected) — or
  `reason: "invalid_encoding"` when `content_encoding` is unrecognised or
  `content` is not valid base64. The allowlist is enforced **only** on the base64
  ingest path; a plain (`utf8`) write is unrestricted.
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
  (int, bytes), `etag` (opaque content SHA-256), `modified_at` (working-tree
  filesystem mtime, `os.Stat().ModTime()`, RFC3339 **nanosecond** precision, UTC).
  `modified_at` shares its semantics with `list_files.modified_at` **by design** —
  the same path returns the **same string** in both (and in `list_files`
  summaries) at the same moment, available immediately after a write without
  waiting for the background git commit. (Source: `internal/tools/summary.go`;
  `docs/conventions/frontmatter.md`.)
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

### 4.13 `move_file`
- **Purpose:** rename or move a file **within one project** as a single atomic,
  history-preserving git commit — a **pure path change**. Same-namespace,
  same-project only; cross-project move is not supported.
  > **Inbound-link rewriting is currently DISABLED** (2026-06-03). A move no longer
  > updates internal Markdown links that point at the moved file; `links_rewritten`
  > is **always `0`**. The field is retained in the response shape so re-enabling
  > link auto-update later (once a reverse-link index exists) is additive, not a
  > breaking change. See backlog B-33.
- **Input:** `namespace` (opt), `project_name` (**required**), `source_path`
  (**required**), `target_path` (**required**), `if_match` (string, optional —
  dual semantic: validates the **target's** etag when the target already exists
  (explicit overwrite), otherwise the **source's** etag).
- **Output (success):** `new_etag` (string — the destination's content etag),
  `links_rewritten` (int — **always `0`**: link auto-update on move is disabled,
  see Purpose; field retained for forward compatibility), `message`.
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_etag: <etag>`. A target that already exists with **no** `if_match` is
  refused (a move never silently overwrites); the conflict carries the target's
  etag. A stale `if_match` carries the relevant file's current etag (see § 5).
- **Output (project-state refusal):** `isError: true`; structured
  `reason: "corrupted" | "dangerous" | "write_disabled"` (see § 7).
- **Side effects:** one synchronous atomic operation (write destination, remove
  source) + a single WAL `move` entry; one background git commit
  (`Move <source> -> <target>`, history-preserving via blob identity) and a
  **`file_moved`** webhook with `commit_hash` follow asynchronously. A
  `file.move` NOTIFY (carrying `source_path` and `path`) is dispatched to other
  `/ws/ui` connections. **No other file's content changes** — a move is a pure
  rename; inbound-link rewriting is disabled (see Purpose). The goldmark-based
  rewriter is retained dormant for future re-enablement.
  (Source: `internal/tools/file.go`; `internal/storage/move.go`,
  `linkrewrite.go`, `commit.go`.)

### 4.14 `append_to_file`
- **Purpose:** insert text into a file **without resending the whole file** — the
  partial-edit complement to `write_file` for large append-mostly source-of-truth
  (`backlog.md`, `journal.md`). The splice is computed **server-side on the file's
  current faithful bytes under the per-file lock**; only the inserted fragment (and
  the anchor, if any) is LLM-mediated. (Backlog B-36.)
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `content` (string, **required** — inserted **verbatim**; the
  server adds **no** newline, so the caller owns all newline placement),
  `position` (string, opt — `end` (default) | `before` | `after`), `anchor`
  (string — **required** when `position` is `before`/`after`, and **rejected**
  when `position` is `end`), `if_match` (string, opt — see § 5).
- **Anchor uniqueness (structural):** for `before`/`after`, `anchor` must occur in
  the file **exactly once**. **Zero matches → error** (`anchor not found in file`);
  **two or more → error** (`anchor is ambiguous: N matches; include more
  surrounding context to make it unique`). The server **never guesses** which
  match was meant — pass enough surrounding context to be unique.
- **Newline semantics:** `content` is inserted byte-for-byte at the chosen point —
  end-append concatenates it at EOF; `before`/`after` insert it immediately
  before/after the anchor occurrence. No separator is injected; if you want a
  trailing newline, include it in `content`.
- **Output (success):** `message` (string), `etag` (string — the new content etag
  of the **whole** file).
- **Output (anchor/argument error):** `isError: true`; human-readable `text`
  (anchor not-found / ambiguous; missing-anchor-for-before/after;
  anchor-with-end). These are **not** conflicts and carry no `current_etag`.
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_etag: <etag>` (stale `if_match`; see § 5).
- **Output (project-state refusal):** `isError: true`; structured
  `reason: "corrupted" | "dangerous" | "write_disabled"` (see § 7).
- **Side effects:** identical to `write_file` — synchronous atomic file write +
  WAL append; a background git commit (`Update <path>`) and a **`file_written`**
  webhook with `commit_hash` follow; a `file.write` NOTIFY is dispatched to other
  `/ws/ui` connections. An append/patch is, to every observer, **just a file
  write** (no new commit kind, no new NOTIFY kind). (Source:
  `internal/tools/partialedit.go`; `internal/storage/partialedit.go`,
  `fs_git.go` `writeTransformed`.)

### 4.15 `patch_file`
- **Purpose:** replace **one unique occurrence** of `old_string` with `new_string`
  (`str_replace`-style) — the partial-edit complement to `write_file` for flipping
  a `status:` line or updating a single paragraph without resending the whole file.
  The replace is computed **server-side on the file's current faithful bytes under
  the per-file lock**; only `old_string` and `new_string` are LLM-mediated.
  (Backlog B-36.)
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `old_string` (string, **required** — must be non-empty and occur
  **exactly once**), `new_string` (string, **required** — **may be empty** to
  delete the matched span), `if_match` (string, opt — see § 5).
- **Uniqueness (structural):** `old_string` must occur **exactly once**. **Zero
  matches → error** (`old_string not found in file`); **two or more → error**
  (`old_string is ambiguous: N matches; include more surrounding context to make
  it unique`). The server **never guesses**. The required exact match of
  `old_string` is itself a positional integrity check — the patch applies only if
  the expected text is actually there.
- **Output (success):** `message` (string), `etag` (string — the new content etag
  of the **whole** file).
- **Output (match/argument error):** `isError: true`; human-readable `text`
  (`old_string` not-found / ambiguous / empty). Not a conflict; no `current_etag`.
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_etag: <etag>` (stale `if_match`; see § 5).
- **Output (project-state refusal):** `isError: true`; structured
  `reason: "corrupted" | "dangerous" | "write_disabled"` (see § 7).
- **Side effects:** identical to `write_file` (see § 4.14 side effects — same WAL
  entry, background `Update <path>` commit, `file_written` webhook, `file.write`
  NOTIFY). (Source: `internal/tools/partialedit.go`;
  `internal/storage/partialedit.go`, `fs_git.go` `writeTransformed`.)

> **Byte-fidelity (B-16) note for `append_to_file`/`patch_file`.** These tools
> **reduce** but do not eliminate the LLM byte-fidelity risk. Homoglyph /
> Unicode-normalization substitution happens *inside the model* before the tool
> call, so no server-side check can detect a substitution the model already made
> in the fragment it sends — and there is deliberately **no `sha256` argument** (a
> whole-file hash would require the caller to have built the whole file, defeating
> the purpose; a fragment hash would cover the same already-corrupted bytes and
> catch nothing — see backlog B-16/B-36). What these tools *do* deliver is **range
> reduction**: the bytes the model must re-utter shrink from the whole document to
> the changed span, cutting the exposed surface by orders of magnitude. `if_match`
> and the exact `old_string`/`anchor` match are the integrity guarantees; both are
> stronger than a fragment hash and free.

### 4.16 `recover_project`
- **Purpose:** recover a project stuck in `corrupted` (uncommitted working-tree
  drift) by **re-syncing its write-path baseline to the actual on-disk git HEAD** and
  clearing a **false** corrupted flag. The recovery for a project that an external
  HEAD move (a host `git reset`, an out-of-band `git add` landing/revert) stranded as
  unwritable even though the working tree is clean. **Non-destructive:** it neither
  commits nor discards working-tree content.
- **Input:** `namespace` (opt), `project_name` (**required**).
- **Output:** `namespace`, `project`, `state` (`healthy` | `corrupted` |
  `dangerous`), `recovered` (bool — true iff now healthy/writable), `message`.
- **Behaviour:** a clean-on-disk project is restored to `healthy` and writes are
  re-enabled. A project with **genuine** uncommitted drift stays `corrupted`
  (`recovered:false`) and the message directs the operator to the Web UI recover
  action's destructive modes (`accept-working-tree` to adopt, `accept-head` to
  discard). A `dangerous` project (unreadable `.git`) is not recoverable over MCP.
- **Side effects:** no commit and no webhook; it may rebuild the project's disposable
  catalog from HEAD. (Source: `internal/tools/recover.go`;
  `internal/storage/recovery.go` `ResyncToHead`; `internal/storage/drift.go`
  `DetectDrift`.)

### 4.17 `translate_file` — RETIRED
- **Status:** **removed** (2026-06-17, B-28). The tool registration and handler
  were deleted, along with the `services.google_cloud` config item.
- **Why:** it was translate-to-WRITE only (it committed a `<base>.<lang><ext>`
  sibling into the repo). Once external file drag-and-drop ADD landed
  (`8495ecc`), on-Shoka editing became the emergency-only path, so authoring —
  including any translation — happens externally and the dropped file is added
  directly. An on-server translate-to-write tool is therefore vestigial. No
  translate-to-read replacement was added (the browser's own translation
  suffices). The `internal/translation/` package code is retained on disk but
  un-wired (dormant), pending a possible future revisit.

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
`cmd/shoka/main.go:47-57`.)

- **HTTP method/target:** `POST` to each subscribed `url`.
- **Headers:**
  - `Content-Type: application/json`
  - `X-Shoka-Signature: <lowercase hex HMAC-SHA256>` — present **only** when the
    hook has a `secret`.
  - Go HTTP client defaults: `User-Agent: Go-http-client/1.1`,
    `Accept-Encoding: gzip`, `Content-Length`.
- **Body schemas** (Source: `internal/webhooks/webhooks.go:26-33`):
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
- **HMAC recipe:** `X-Shoka-Signature =
  lowercasehex(HMAC_SHA256(key=<hook secret>, msg=<exact raw request body bytes>))`.
  No canonicalization — sign/verify the exact bytes. Shell:
  `printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex`.
- **Delivery semantics:** asynchronous and best-effort; the originating MCP call
  never blocks on or fails because of delivery. **2 attempts**, **200 ms** backoff
  (doubling), failures are logged and never propagated. In-flight deliveries are
  drained on graceful shutdown. (Source: `internal/webhooks/webhooks.go:62-123`.)

---

## 7. Storage model

(Source: `internal/storage/`.)

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

### 7.1 Observability — the `/metrics` endpoint

An **optional Prometheus endpoint** for operators. It adds **no wire-visible
behaviour** to the MCP protocol — it is a separate, out-of-band scrape surface.

- **Off by default, loopback-only:** the endpoint is unregistered unless
  `metrics.addr` is set; a non-empty `metrics.addr` is forced to a loopback host
  (the pprof endpoint, `--profile-addr`, follows the same defaults). It is
  pull-based Prometheus text exposition over a private registry (no default
  Go/process collectors), so scraping it is opt-in and not exposed publicly.
- **Coverage — 33 metric families across five subsystem groups:** **storage/WAL**
  (WAL depth/bytes/age, write-disabled state, background-commit counts, file-lock
  leases, per-project state, catalog and quarantine counters), the derivative
  **index** (index health, repair-sweep rebuilds, content-search fast-path
  outcomes, fix-links rewrites/conflicts), **lost+found** (sweep passes, per-file
  dispose/move actions, skipped-project states), **OAuth** (active connections,
  tokens issued, revocations — present only when OAuth is enabled), and **notify**
  (slow-subscriber drops). The **live endpoint is the source of truth** for the
  exact family list; this contract names the groups, not all 33 families.
- **Privacy guarantee (contract-level):** **no metric label carries a secret,
  token, id, path, or domain.** Labels are bounded enums (e.g. `result`, `reason`,
  `operation`, `outcome`, `action`, `state`, `source`) and per-project dimensions
  (`namespace`, `project`) only. The OAuth families in particular expose **counts
  and one gauge only** — never a series id, token, refresh, code, PKCE value,
  principal, or client domain (the source is type-constrained so a secret cannot
  reach a label by construction). Scraping `/metrics` therefore cannot leak
  document content, identities, or credentials.

(Source: `internal/metrics/metrics.go:56-84,145-360`; `internal/config/config.go:186-191`;
`cmd/shoka/main.go:269-274,524-543`.)

---

## 8. What this contract does NOT cover

- **History rewriting** (e.g. `git filter-repo`) is **not** exposed through MCP. It
  is an operator procedure — see `docs/operations/sensitive-data-removal.md`.
- **MCP resource subscriptions / server push** are not implemented. Use **webhooks**
  for change notification. (No resources/subscriptions are registered in
  `cmd/shoka/main.go`.)
- **Diffing arbitrary commits** is not a tool. Fetch two versions with
  `read_file_at_version` and diff client-side.
- **No multi-tenancy beyond namespaces**, and **no per-namespace ACLs**.
  Authentication is a single shared Bearer-token set (§ 3); it does not scope
  access per namespace.

---

## Sources

- **Source files:** `cmd/shoka/main.go:75-216` (transport, tool registration,
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
