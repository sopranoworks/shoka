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
`go vet ./...`, and `go test ./...` all pass. (Source: `.devcontainer/Dockerfile`;
`meta/reports/2026-05-28-shoka-schema-fixes-complete.md` § Build/test status.)

## Configuration reference

Configuration is a YAML file (Source: `internal/config/config.go:11-69`). A fully
annotated example is `shoka.example.yaml`.

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `storage.base_dir` | string | **yes** | — | Directory holding project repos (`<base_dir>/<namespace>/<project>`). Relative paths resolve to the working dir; created on startup. |
| `server.http.listen` | string | **yes** | — | Address for the web UI + WebSocket endpoints (`/`, `/ws/ui`, `/drafts/...`). |
| `server.mcp.listen` | string | **yes** | — | Address for the MCP (SSE) endpoint. |
| `server.http.external_url` | string | no | "" | Public URL reported by `get_server_info`. |
| `server.mcp.external_url` | string | no | "" | Public URL reported by `get_server_info`. |
| `server.http.tls.enabled` / `.cert_file` / `.key_file` | bool / string | no | false | TLS for the web listener. Same shape under `server.mcp.tls`. |
| `server.auth.enabled` | bool | no | `false` | Enable Bearer-token auth. When false, no auth and all WS origins accepted. |
| `server.auth.tokens` | list of strings | no | [] | Accepted bearer tokens (constant-time compared). |
| `server.auth.allowed_origins` | list of strings | no | [] | When auth is on, permitted WebSocket `Origin` values (empty Origin rejected; MCP/SSE is not origin-checked). |
| `services.google_cloud.project_id` | string | no | "" | When set, registers `translate_file` (uses Application Default Credentials). |
| `webhooks[].name` / `.url` / `.events` / `.secret` | strings / list | no | — | Outbound webhook subscriptions. `events` ⊆ {`file_written`,`file_deleted`,`project_created`}. `secret` enables the `X-Shoka-Signature` HMAC header. |

Validation: the server refuses to start without `storage.base_dir`,
`server.http.listen`, and `server.mcp.listen`. (Source:
`internal/config/config.go:58-69`.)

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
| HTTP **401** on the MCP endpoint | Auth enabled; request lacks a valid `Authorization: Bearer`. Note `?token=` is **not** accepted on MCP/SSE — header only. |
| WebSocket upgrade **401** | Auth on, no token. Pass the token via `?token=` (allowed on `/ws/ui`, `/drafts/`) or the header. |
| WebSocket upgrade **403** | Auth on and the request `Origin` is not in `allowed_origins` (empty Origin is rejected). |
| `translate_file` tool missing | `services.google_cloud.project_id` is unset, so the tool is not registered. |
| `write_file`/`delete_file` returns a version conflict | Another writer changed the file since you read it. Re-read to get the current `version`, then retry (contract § 5). |
| Webhook never arrives | Check the hook's `events` includes the event, the `url` is reachable, and server logs (delivery is best-effort: 2 attempts, then logged failure). |
| Port already in use on startup | Another process holds `http.listen`/`mcp.listen`; change the port or stop the other process. |

## Sources

- Source: `internal/config/config.go:11-69` (schema + validation),
  `cmd/server/main.go:29-116,200-216` (startup, listeners, conditional translate,
  auth wiring), `internal/storage/fs_git.go:64-118` (base_dir creation, project
  repos), `.devcontainer/Dockerfile`.
- Documents: `shoka.example.yaml` (annotated config), `docs/contracts/mcp-v1.md`
  (§ 1 versioning, § 3 auth, § 6 webhooks),
  `meta/reports/2026-05-28-shoka-schema-fixes-complete.md` (build/test status).
