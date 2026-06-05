# Shoka Operations

How to run, configure, and maintain Shoka. For the design see
[`docs/ARCHITECTURE.md`](ARCHITECTURE.md); for the wire interface see
[`docs/contracts/mcp-v1.md`](contracts/mcp-v1.md).

## Running

```sh
go build -o shoka ./cmd/server
./shoka --config shoka.yaml
```

The `--config` flag defaults to `shoka.yaml`. On startup Shoka creates
`storage.base_dir` if absent and starts two listeners (web + MCP). (Source:
`cmd/server/main.go:30-35`; `internal/storage/fs_git.go:64-76`.)

### Devcontainer

A devcontainer is provided at `.devcontainer/` (base image
`mcr.microsoft.com/devcontainers/go:1-bookworm`; Go's toolchain management fetches
the exact patch from `go.mod` at build time). Inside it, `go build ./...`,
`go vet ./...`, and `go test ./...` all pass. (Source: `.devcontainer/Dockerfile`.)

## Configuration reference

Configuration is a YAML file (Source: `internal/config/config.go`). A fully
annotated example is `shoka.example.yaml` — the canonical reference for every key
and its default. The schema has **eleven top-level sections**: `server`,
`identity`, `storage`, `services`, `filelock`, `wal`, `wal_worker`, `notify`,
`metrics`, `catalog`, and `webhooks`. Only three keys are required
(`server.http.listen`, `server.mcp.listen`, `storage.base_dir`); every other
section is optional and falls back to a built-in default.

### `server` — listeners, auth, logging

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `server.http.listen` | string | **yes** | — | Address for the web UI + WebSocket endpoints (`/`, `/ws/ui`, `/drafts/...`). |
| `server.mcp.listen` | string | **yes** | — | Address for the MCP (Streamable HTTP) endpoint; clients connect at the `/mcp` path. |
| `server.http.external_url` / `server.mcp.external_url` | string | no | "" | Public URL reported by `get_server_info`; `server.mcp.external_url` is also the OAuth public origin (see *Enabling OAuth*). |
| `server.http.tls.enabled` / `.cert_file` / `.key_file` | bool / string | no | false | TLS for the web listener. Same shape under `server.mcp.tls`. |
| `server.auth.enabled` | bool | no | `false` | Enable Bearer-token auth. When false, no auth and all WS origins accepted. |
| `server.auth.tokens` | list of strings | no | [] | Accepted bearer tokens (constant-time compared). |
| `server.auth.allowed_origins` | list of strings | no | [] | When auth is on, permitted WebSocket `Origin` values (empty Origin rejected; the MCP endpoint is bearer-authenticated, not origin-checked). |
| `server.auth.oauth.*` | block | no | off | Built-in OAuth 2.1 authorization server. See *Enabling OAuth* below; keys are `enabled`, `consent_credential`, `trusted_client_metadata_domains`, `access_token_ttl`, `refresh_token_ttl`, `authorization_code_ttl`. |
| `server.log.level` / `server.log.format` | string | no | `info` / `text` | Structured logging — see *Logging* below. |

### `identity` — commit author (single-user, provisional)

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `identity.user.name` / `.email` | string | no | `Shoka Operator` / `operator@shoka.local` | The one operator recorded as git Committer and `Shoka-User` trailer on every commit. |
| `identity.agent_default.name` / `.worker` | string | no | `shoka-agent` / "" | Fallback author identity for MCP clients that declare no `clientInfo` name / worker id. |

Provisional single-user mode (maintenance backlog B-28); there is no
authentication here. (Source: `internal/config/config.go:193-210`.)

### `storage` — data root + background workers

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `storage.base_dir` | string | **yes** | — | Directory holding project repos (`<base_dir>/<namespace>/<project>`). Relative paths resolve to the working dir; created on startup. |
| `storage.drift_scan.on_startup` | bool | no | `true` | Run a working-tree-vs-git drift scan once at startup (marks projects healthy/corrupted/dangerous). |
| `storage.drift_scan.interval` | duration | no | `0` | Periodic re-scan cadence; `0` disables periodic re-scan. |
| `storage.lost_found.enabled` / `.interval` | bool / duration | no | `true` / `5m` | Lost+found worker: per healthy project, deletes untracked `shoka.disposable` files and moves the rest to a per-project lost+found area, restoring the tracked-only invariant. |
| `storage.index.enabled` / `.interval` | bool / duration | no | `true` / `5m` | Derivative-index repair worker: reconciles each project's `index.db` with HEAD, rebuilding from working-tree bytes when stale/missing/corrupt. |

### `services` — optional integrations

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `services.google_cloud.project_id` | string | no | "" | When set, registers `translate_file` (uses Application Default Credentials). |

### `filelock`, `wal`, `wal_worker`, `notify` — write pipeline tunables

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `filelock.max_lease_duration` / `.reaper_interval` | duration | no | `5m` / `1s` | Per-file write-lock lease length and the stale-lease reaper cadence (reads take no lock). |
| `wal.max_entries` | int | no | `1000` | Write-ahead-log depth threshold; once exceeded, writes are refused (write-disabled mode) until the WAL drains. |
| `wal_worker.min_workers` / `.max_workers` | int | no | `1` / `8` | Bounds of the background git-commit worker pool that drains the WAL (per-project order preserved). |
| `wal_worker.idle_timeout` / `.scan_interval` | duration | no | `30s` / `100ms` | Worker idle-shutdown timeout and WAL poll cadence. |
| `wal_worker.backoff_initial` / `.backoff_max` | duration | no | `100ms` / `30s` | Commit-retry backoff bounds. |
| `notify.max_entries` | int | no | `1000` | Ring-buffer size of the in-process notification center (recent storage-activity events). |

### `metrics`, `catalog`

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `metrics.addr` | string | no | "" | Prometheus `/metrics` endpoint address; empty = off, non-empty is forced to a loopback host. See *Scraping `/metrics`* below. |
| `catalog` | block | no | `{}` | Per-project bbolt catalog cache; currently no tunable fields (the section reserves space for future knobs). |

### `webhooks` — outbound subscriptions

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `webhooks[].name` / `.url` / `.events` / `.secret` | strings / list | no | — | Outbound webhook subscriptions. `events` ⊆ {`file_written`,`file_deleted`,`project_created`}. `secret` enables the `X-Shoka-Signature` HMAC header. |

Validation: the server refuses to start without `storage.base_dir`,
`server.http.listen`, and `server.mcp.listen`, and rejects an invalid
`server.log.level`/`format` or a `wal_worker` min/max inversion. (Source:
`internal/config/config.go:299-329`.)

## Logging

Shoka emits structured log lines to **stderr**; stdout is reserved for the MCP
transport's stream output and must remain clean.

### Configuration

Add a `server.log` block to `shoka.yaml`:

```yaml
server:
  log:
    level: info    # error | warn | info | debug  (default: info)
    format: text   # text | json                  (default: text)
```

An absent `server.log` block is fully backward-compatible: the server starts at
`info`/`text` without any config change.

| Key | Values | Default | Effect |
|-----|--------|---------|--------|
| `server.log.level` | `error` `warn` `info` `debug` | `info` | Minimum severity to emit. |
| `server.log.format` | `text` `json` | `text` | Human-readable key=value (`text`) or machine-parseable JSON (`json`). Use `json` when shipping logs to a structured collector. |

### What is logged

| Level | Events |
|-------|--------|
| `error` | The `tools/call`-during-initialization rejection (from the SDK — the session-init fault), tool handler errors/panics, storage commit failures. |
| `warn` | Requests rejected with HTTP status ≥ 400 (unknown/expired session, auth failure), webhook delivery failures. |
| `info` (default) | Server start/stop and listener addresses; server→client stream open/close and session termination; MCP session lifecycle (session connected/disconnected, via the SDK); tool-call received/completed with outcome; git commits (hash + path); webhook delivery success. |
| `debug` | Everything at `info`, **plus** the per-message JSON-RPC method + session ID for each POST to the MCP endpoint, and finer SDK protocol detail. |

**Logs never contain file content or auth tokens** — only metadata (paths,
method names, session IDs, outcome labels). This is enforced by design: the
logging layer never receives content or credential values.

#### Protocol-level output at `debug`

At `server.log.level: debug` the MCP endpoint additionally emits redacted,
protocol-level traces to stderr to make wire-level faults diagnosable:

- `mcp message received` — each inbound JSON-RPC request (POST): `rpc_method`,
  `rpc_id`, `conn_id`, `session_id`, and `rpc_params` (the full params as JSON).
  The `write_file` `content` argument is replaced with `<redacted N bytes>`;
  everything else is verbatim.
- `mcp response sent` — each outbound JSON-RPC response (the POST response, whether
  the SDK answers with `application/json` or a `text/event-stream` frame): `rpc_id`
  and the full response `event_data`. `read_file` / `read_file_at_version`
  `content` and `read_summary` `excerpt` are replaced with `<redacted N bytes>`;
  everything else (including etags, commit hashes, and error messages) is verbatim.
- `mcp session established` — logged when the `initialize` response assigns a
  Streamable HTTP `Mcp-Session-Id`; carries that `session_id` for correlation.
- `mcp stream opened` / `mcp stream closed` — the optional standalone server→client
  SSE stream (GET) opening and closing.
- `mcp session terminated` — a client `DELETE` ending its session.
- `mcp event sent` — any other server→client stream frame (a notification, ping,
  etc.): only its `event_name` and `data_bytes` size are logged, never raw payload.
- Session lifecycle (`server session connected`, `session initialized`,
  `server session disconnected`) is emitted by the MCP SDK itself via the
  configured logger.

This output is best-effort diagnostic instrumentation only; it never changes the
wire protocol, and file contents and bearer tokens are never logged. It is
verbose — enable `debug` for diagnosis, not for steady-state operation.

### Diagnosing MCP session faults

If the MCP client fails to complete its handshake, run the server with
`level: debug`. The debug stream will show:

1. `mcp message received` — the inbound `initialize` POST and every later request.
2. `mcp response sent` — the matching responses.
3. `mcp session established` — the `Mcp-Session-Id` the server assigned at
   `initialize`.
4. SDK session lifecycle events (session started, capability negotiation, etc.).
5. Tool-call received/completed entries for any tool invocations that succeed.

A `request rejected ... status=404` with a `session_id` is the **stale-session**
signal: the client presented a session id this process does not know (typically
because Shoka restarted). The client should re-initialize automatically; see § 2
of `docs/contracts/mcp-v1.md`. Comparing these events against the Streamable HTTP
flow in that section pinpoints where a session diverges.

## Backup

A project is an ordinary Git repository under `base_dir`. Back up `base_dir` as you
would any set of Git repositories (filesystem snapshot, or `git clone`/`git bundle`
per project). No database to dump. (Source: `internal/storage/fs_git.go:91-118`.)

## Upgrading

Shoka's MCP interface is versioned (see `docs/contracts/mcp-v1.md` § 1). Adding a
new optional argument, response field, or tool is **non-breaking**. Removing or
renaming a tool/field, or making an optional argument required, is **breaking** and
requires a new contract version. Treat the contract's stability rules as the
upgrade compatibility policy.

## Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| Server exits immediately with `... is required` | Missing `storage.base_dir`, `server.http.listen`, or `server.mcp.listen`. (`internal/config/config.go:58-69`.) |
| HTTP **401** on the MCP endpoint | Auth enabled; request lacks a valid `Authorization: Bearer`. Note `?token=` is **not** accepted on the MCP endpoint — header only. |
| HTTP **404** on the MCP endpoint (`request rejected ... status=404` with a `session_id`) | The client presented an `Mcp-Session-Id` this process does not know (normal after a Shoka restart). Expected and self-healing: the client re-initializes. (Contract § 2.) |
| HTTP **403** `invalid Host header` on the MCP endpoint | DNS-rebinding protection: a non-loopback `Host` reached a loopback-bound Shoka (often a reverse proxy forwarding the original `Host`). Fix the proxy `Host`, or start with `MCPGODEBUG=disablelocalhostprotection=1`. (Contract § 2.) |
| WebSocket upgrade **401** | Auth on, no token. Pass the token via `?token=` (allowed on `/ws/ui`, `/drafts/`) or the header. |
| WebSocket upgrade **403** | Auth on and the request `Origin` is not in `allowed_origins` (empty Origin is rejected). |
| `translate_file` tool missing | `services.google_cloud.project_id` is unset, so the tool is not registered. |
| `write_file`/`delete_file` returns a conflict | Another writer changed the file since you read it. Re-read to get the current `etag` (the content-SHA-256 token), pass it as `if_match`, then retry (contract § 5). |
| Webhook never arrives | Check the hook's `events` includes the event, the `url` is reachable, and server logs (delivery is best-effort: 2 attempts, then logged failure). |
| Port already in use on startup | Another process holds `http.listen`/`mcp.listen`; change the port or stop the other process. |

## Sources

- Source: `internal/config/config.go:11-69` (schema + validation),
  `cmd/server/main.go:29-116,200-216` (startup, listeners, conditional translate,
  auth wiring), `internal/storage/fs_git.go:64-118` (base_dir creation, project
  repos), `.devcontainer/Dockerfile`.
- Documents: `shoka.example.yaml` (annotated config), `docs/contracts/mcp-v1.md`
  (§ 1 versioning, § 3 auth, § 6 webhooks).
