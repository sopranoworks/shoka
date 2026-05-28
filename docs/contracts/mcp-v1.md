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

- This document describes **Shoka MCP v1**, derived from the codebase reachable
  from `master` as of 2026-05-28 (after the remediation, verification, and
  schema-fixes directives).
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

- **Protocol:** MCP over **SSE**, spec `2024-11-05`. This is *not* the
  streamable-HTTP transport. (Source: `cmd/server/main.go:86` —
  `mcp.NewSSEHandler(...)`.)
- **Listen address:** from config `server.mcp.listen` (e.g. `:8081`). The handler
  is mounted as the MCP server's root handler and is **path-agnostic** — a GET to
  any path opens the stream. (Source: `cmd/server/main.go:86-105`.)
- **Handshake** (verified, verification report § 6 Task A): the client opens the
  stream with `GET <any path>`; the server's first SSE event advertises the
  message endpoint:

  ```
  event: endpoint
  data: /sse?sessionid=<SESSION_ID>
  ```

- **Message channel:** the client POSTs JSON-RPC messages to the advertised
  `…?sessionid=<SESSION_ID>` endpoint (relative to the GET path).

(Source: verification report `meta/reports/2026-05-27-shoka-verification.md` § 2.1
and § 6 Task A.)

---

## 3. Authentication

Controlled by config `server.auth` (Source: `internal/config/config.go:23-30`;
`internal/auth/auth.go`).

- **`server.auth.enabled: false` (default):** no authentication; all requests pass
  and all WebSocket origins are accepted. (Source: `internal/auth/auth.go:39-44`.)
- **`server.auth.enabled: true`:**
  - **MCP/SSE requires `Authorization: Bearer <token>`** on every request. The
    `?token=` query parameter is **NOT** accepted on MCP/SSE — only on the
    WebSocket paths `/ws/ui` and `/drafts/`. (Source: `internal/auth/auth.go:39-75`
    header-only `Authenticate`/`Middleware` vs. query-allowing
    `AuthenticateWebSocket`/`MiddlewareAllowQueryToken`; wired at
    `cmd/server/main.go:104` (MCP, header-only) and `:215-216` (WS, query-allowing).
    This was finding **F1**, fixed:
    `meta/reports/2026-05-28-shoka-schema-fixes-complete.md`.)
  - Tokens are compared in **constant time** against the configured set. (Source:
    `internal/auth/auth.go:80-89`.)
  - **Failure response:** HTTP **401** with header `WWW-Authenticate: Bearer` and
    body `unauthorized`. (Source: `internal/auth/auth.go` middleware; verification
    report § 6 Task A.)
  - The credential must be present on **every** request of the session — both the
    GET stream and each POST to the message endpoint. (The SSE client must attach
    the header to all requests.)

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
- **`expected_version`** (write/delete only) — optional; when omitted or `""`,
  the optimistic-locking check is skipped. See § 5. (Source:
  `internal/storage/fs_git.go:150-158`, `:284-292`.)
- **`path`** — project-relative. Absolute paths and `..` traversal are rejected.
  (Source: `internal/storage/fs_git.go:142-145` and parallel checks.)
- **Errors** surface as a **tool-level result with `isError: true`** and a
  human-readable `content[0].text`; they are not JSON-RPC protocol errors. The
  one structured error is the version conflict (§ 5). (Source: handlers in
  `internal/tools/*.go`; verification report § 2.2.)
- **Side effects:** read-only tools produce no commit and no webhook. Mutating
  tools commit and emit a webhook (§ 6). Listed per tool below.

### 4.1 `get_server_info`
- **Purpose:** report the server's configured public URLs and storage location.
- **Input:** none. (Source: `internal/tools/info.go:10-11`.)
- **Output:** `external_url`, `http_listen`, `mcp_listen`, `storage_base_dir`
  (all strings). (Source: `internal/tools/info.go:13-18`.)
- **Errors:** none expected. **Side effects:** none.

### 4.2 `list_projects`
- **Purpose:** list project names in a namespace.
- **Input:** `namespace` (string, optional, default `"default"`).
- **Output:** `projects` (array of strings; empty array if the namespace has no
  projects). (Source: `internal/tools/project.go:55-86`;
  `internal/storage/fs_git.go:229-254`.)
- **Side effects:** none.

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
  versions and/or summaries.
- **Input:** `namespace` (opt), `project_name` (**required**), `path` (opt,
  default project root), `include_versions` (bool, opt, default `false`),
  `include_summaries` (bool, opt, default `false`).
- **Output:** `files` (array; directories carry a trailing `/`); `versions`
  (object: file → current commit hash) present only when `include_versions`;
  `summaries` (object: file → `{frontmatter, heading}`) present only when
  `include_summaries`. `.git` and `.drafts` are never listed. **No status
  filtering** — every file is returned regardless of frontmatter `status` (the
  retired-layer hiding in `docs/conventions/document-lifecycle.md` is a
  client-side convention). (Source: `internal/tools/project.go:88-175`;
  `internal/storage/fs_git.go:337-374`.)
- **Side effects:** none.

### 4.5 `read_file`
- **Purpose:** read a file's full content plus its current version.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**).
- **Output:** `content` (string), `version` (string — current commit hash;
  `""` if the file has no commit history). (Source: `internal/tools/file.go:15-62`;
  `internal/storage/fs_git.go:473-516`.)
- **Errors:** missing file → `isError: true`. **Side effects:** none.

### 4.6 `read_file_at_version`
- **Purpose:** read a file's content as of a specific commit (e.g. to recover a
  deleted or superseded version).
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `commit_hash` (string, **required**).
- **Output:** `content` (string).
- **Errors:** unknown hash or path-not-in-that-commit → `isError: true`.
  **Side effects:** none. (Source: `internal/tools/file.go:255-295`;
  `internal/storage/fs_git.go:430-471`.)

### 4.7 `write_file`
- **Purpose:** create or overwrite a file; commit atomically.
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `content` (string, **required**), `expected_version` (string,
  optional — omit/`""` to skip locking; see § 5).
- **Output (success):** `message` (string), `version` (string — new commit hash).
- **Output (conflict):** `isError: true`; structured `conflict: true`,
  `current_version: <hash>` (see § 5).
- **Side effects:** atomic Git commit (`Update <path>`); emits a **`file_written`**
  webhook with `commit_hash`. (Source: `internal/tools/file.go:64-118`;
  `internal/storage/fs_git.go:120-204`.)

### 4.8 `delete_file`
- **Purpose:** remove a file from the current tree (`git rm`); commit atomically.
  History is preserved — content remains recoverable (§ 7, storage model).
- **Input:** `namespace` (opt), `project_name` (**required**), `path`
  (**required**), `expected_version` (string, optional — omit/`""` to skip
  locking).
- **Output (success):** `message`, `version` (delete commit hash).
- **Output (conflict):** `isError: true`; `conflict: true`, `current_version`
  (see § 5).
- **Side effects:** `git rm` + atomic commit (`Delete <path>`); emits a
  **`file_deleted`** webhook with `commit_hash`. (Source:
  `internal/tools/file.go:120-170`; `internal/storage/fs_git.go:256-335`.)

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
  (int, bytes), `version` (last commit hash, omitted if none), `modified_at`
  (RFC3339, omitted if none). (Source: `internal/tools/summary.go:14-72`;
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

## 5. Optimistic locking

(Source: `internal/storage/fs_git.go:53-62,126-158,262-292`; verification report
§ 2.2; `meta/reports/2026-05-28-shoka-schema-fixes-complete.md`.)

- A file's **version** is the hash of the most recent commit that touched it
  (`""` if it has no history). (Source: `internal/storage/fs_git.go:473-516`.)
- **To enforce locking:** pass `expected_version` (obtained from `read_file` or a
  prior write) to `write_file`/`delete_file`. If it does not equal the file's
  current version, the call is rejected with a conflict and **no change is made**.
- **To skip locking:** omit `expected_version` or pass `""` (a blind write).
- **Conflict response shape** (copy verbatim — verification report § 2.2):

  ```json
  {
    "content": [
      { "type": "text", "text": "version conflict: file is now at <current_hash> (you expected <your_hash>); re-read the file and retry with the current version" }
    ],
    "structuredContent": {
      "conflict": true,
      "current_version": "<current_hash>",
      "message": ""
    },
    "isError": true
  }
  ```

  - It is a **tool-level error** (`isError: true`), **not** a JSON-RPC error.
  - `current_version` is **omitted** when the file does not currently exist
    (current version is empty).
- **Recovery procedure:** on conflict, call `read_file` to get the current
  `version`, reconcile your change against the current content, then retry the
  write with the new `expected_version`.

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

(Source: `internal/storage/fs_git.go`.)

- Files live at **`<base_dir>/<namespace>/<project>/<path>`**. (`fs_git.go:78-89`.)
- **Each project is its own Git repository** (`git.PlainInit`). (`fs_git.go:91-118`.)
- **Every `write_file` is an atomic commit** (`Update <path>`); **every
  `delete_file` is an atomic commit** with `git rm` semantics (`Delete <path>`).
  Commit author is `MCP Server <mcp-server@shoka.io>`. (`fs_git.go:120-204,256-335`.)
- **History is preserved indefinitely.** Past content of deleted or overwritten
  files is retrievable with `read_file_at_version` against a commit from
  `get_history`. (`fs_git.go:430-471`; see `docs/conventions/document-lifecycle.md`
  § Recovering a deleted file.)

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
