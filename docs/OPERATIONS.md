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
`storage.base_dir` if absent and starts up to three listeners (web, MCP-plain,
MCP-OAuth — see *Connecting clients*). (Source:
`cmd/server/main.go:30-35`; `internal/storage/fs_git.go:64-76`.)

### Devcontainer

A devcontainer is provided at `.devcontainer/` (base image
`mcr.microsoft.com/devcontainers/go:1-bookworm`; Go's toolchain management fetches
the exact patch from `go.mod` at build time). `go build ./...`, `go vet ./...`,
and `go test ./...` are expected to pass inside it; the suite is currently
verified on the host, while the devcontainer test leg has not yet been run on a
Docker-capable environment (maintenance backlog B-12 remains open). (Source:
`.devcontainer/Dockerfile`.)

### Running as a service (systemd / launchd)

Shoka runs in the **foreground** and has no `--daemon`/fork mode — by design. It
handles `SIGTERM`/`SIGINT` with a full graceful shutdown (HTTP in-flight drain →
leftover-relocation wait → WAL worker-pool drain → store/index close → webhook
drain), which is exactly what a service manager needs. So you run Shoka *as a
managed foreground process* under **systemd** (Linux) or **launchd** (macOS), and
let it own backgrounding, restart, and log capture. Two ready-to-edit templates
ship in [`docs/operations/`](operations/): `shoka.service` (systemd) and
`com.shoka.server.plist` (launchd). Every host/path/user value in them is a
placeholder to fill in.

> **The load-bearing setting — `TimeoutStopSec`.** Shoka's graceful stop can take
> up to ~35s (the WAL worker pool alone drains for up to **30s** —
> `internal/storage/fs_git.go:329` — on top of the 5s HTTP drain). The systemd
> unit sets **`TimeoutStopSec=45`** so systemd does not `SIGKILL` Shoka mid-drain
> and leave a half-flushed WAL. **Do not lower it below ~40s.** (macOS launchd has
> no exact equivalent — see the caveat below.)

**systemd (Linux):**

```sh
# Edit the placeholders (binary path, --config path, User/Group, WorkingDirectory):
sudo install -m 0644 docs/operations/shoka.service /etc/systemd/system/shoka.service
sudo systemctl daemon-reload
sudo systemctl enable --now shoka     # start now + on boot
systemctl status shoka
journalctl -u shoka -f                # logs (Shoka -> stderr -> journald)
sudo systemctl stop shoka            # graceful stop within TimeoutStopSec
```

The unit is `Type=simple` (foreground), `Restart=on-failure`, and relies on
systemd's default `SIGTERM` stop signal — the one Shoka handles.

**launchd (macOS):**

```sh
# Edit the placeholders (ProgramArguments, log paths, WorkingDirectory):
cp docs/operations/com.shoka.server.plist ~/Library/LaunchAgents/
launchctl load   ~/Library/LaunchAgents/com.shoka.server.plist   # start
launchctl unload ~/Library/LaunchAgents/com.shoka.server.plist   # stop
```

`RunAtLoad` + `KeepAlive` start and restart Shoka; `StandardErrorPath` captures
its stderr to a log file.

> **macOS caveat (no systemd parity).** launchd `SIGTERM`s then `SIGKILL`s after a
> fixed grace (~20s by default), which is **shorter** than the up-to-30s WAL pool
> drain. A large WAL backlog could be cut short on macOS. The plist sets
> `ExitTimeOut=45` to request more, but launchd may cap it. This is fine for a dev
> or secondary host; the fully-graceful deployment is Linux/systemd.

**Logs.** Under either manager Shoka logs to **stderr**, captured by journald
(systemd) or `StandardErrorPath` (launchd). Alternatively, have Shoka write a
**self-rotating log file** itself by setting `server.log.output: file` in
`shoka.yaml` — see [*Logging*](#logging) (B-66). Either path works under a service
manager; pick one.

## Connecting clients

Shoka serves MCP over **Streamable HTTP** at the `/mcp` path of an MCP transport's
listen address (the plain transport's `server.mcp.plain.listen`, or the external
`server.mcp.oauth.listen`). How a client registers depends on whether it speaks
Streamable HTTP directly:

- **Claude Code** (CLI) registers Shoka directly:

  ```sh
  claude mcp add --transport http shoka http://localhost:8081/mcp
  ```

- **A non-CLI client such as Claude Desktop**, which cannot add a Streamable-HTTP
  server directly, connects through the **`mcp-remote`** bridge. Add an
  `mcpServers` entry to the client's config (`claude_desktop_config.json`) that
  runs `npx mcp-remote http://localhost:8081/mcp` — a direct
  `{"type": "http", "url": ...}` entry is rejected by such clients, but routing
  through `mcp-remote` works.

The `http://localhost:8081/mcp` shown is a **placeholder** matching
`shoka.example.yaml`'s default `server.mcp.plain.listen` (`:8081`); substitute your own
address. (Source: maintenance backlog B-01; `shoka.example.yaml`.)

### Connecting claude.ai behind a CDN / WAF / bot-defense (Cloudflare etc.) — allowlist Anthropic's egress

If you put Shoka behind Cloudflare (or any other CDN / WAF / bot-defense / firewall)
and the **claude.ai** connector fails *after* OAuth appears to succeed, the cause is
almost certainly the edge, **not** Shoka's OAuth code. This is worth recognising
immediately because the error wording sends you in the wrong direction.

**Symptom signature.** The OAuth flow completes normally — discovery, authorize,
`POST /token` returns **200** with a token issued (PKCE matches, everything in the
logs looks healthy). Then claude.ai reports:

> Authorization with the MCP server failed. You can check your credentials and
> permissions. … ofid_…

…and crucially **no authenticated `/mcp` request appears in the server logs at all —
not even at the reverse proxy.** That combination — the last response succeeded, the
very next request never arrived — points at the **edge (CDN/WAF/bot-defense) in front
of the server**, not at Shoka. The "credentials/permissions" wording is misleading:
the token is fine; the request that would have used it was dropped before it reached
anything Shoka or the proxy could log.

**Cause.** After `/token`, Anthropic's broker makes the authenticated MCP request as a
**server-to-server `POST /mcp` with `Authorization: Bearer …` and no browser cookies
or browser fingerprint.** That profile is exactly what a bot-defense product is built
to drop — e.g. Cloudflare **Bot Fight Mode** kills it at the edge, upstream of your
reverse proxy and Shoka.

**Fix — allowlist Anthropic's egress at the edge.** Permit Anthropic's published
egress IP range, which was **`160.79.104.0/21`** as of 2026-06:

- **Cloudflare:** add an **IP Access Rule = Allow** for that range, **and** exempt it
  from **Bot Fight Mode**, the WAF, and any managed/bot challenge.
- **Any other CDN / WAF / firewall:** the equivalent allow rule for that range.

Anthropic publishes (and may update) its current egress ranges — treat their
connector/IP-address documentation as the authoritative source and confirm the
current published range rather than hard-trusting `160.79.104.0/21` forever. The
Cloudflare specifics above are just the concrete instance of the general rule:
**allowlist Anthropic's egress at whatever edge defense sits in front of Shoka.**

(This is an operator/edge-side configuration note; Shoka itself needs no change.)

## Configuration reference

Configuration is a YAML file (Source: `internal/config/config.go`). A fully
annotated example is `shoka.example.yaml` — the canonical reference for every key
and its default. The schema has **eleven top-level sections**: `server`,
`identity`, `storage`, `services`, `filelock`, `wal`, `wal_worker`, `notify`,
`metrics`, `catalog`, and `webhooks`. The required keys are `server.http.listen`,
`storage.base_dir`, and **at least one MCP transport** (`server.mcp.plain.listen`
and/or `server.mcp.oauth.listen` — neither set is a startup error); every other
section is optional and falls back to a built-in default.

### `server` — listeners, auth, logging

| Key | Type | Required | Default | Meaning |
|-----|------|----------|---------|---------|
| `server.http.listen` | string | **yes** | — | Address for the web UI + WebSocket endpoints (`/`, `/ws/ui`, `/drafts/...`). |
| `server.mcp.plain.listen` | string | one MCP transport required† | — | Address for the **plain** (internal) MCP transport; clients connect at the `/mcp` path. |
| `server.mcp.plain.bearer_auth` | bool | no | `false` | Off → the plain transport is **unauthenticated** (loopback/internal use only). On → it requires `Authorization: Bearer <token>` validated against `server.auth.tokens` (an API-Token; **must** be behind TLS — see *TLS*). |
| `server.mcp.oauth.listen` | string | one MCP transport required† | — | Address for the **OAuth** (external) MCP transport. Its presence enables OAuth — there is no separate flag. Purely OAuth; static bearer is not accepted here. See *Enabling OAuth*. |
| `server.mcp.plain.external_url` / `server.mcp.oauth.external_url` | string | no | "" | Public URL for self-references; `server.mcp.oauth.external_url` is the OAuth public origin (see *Enabling OAuth*). `get_server_info` reports the plain transport's listen address. |
| `server.auth.enabled` | bool | no | `false` | Enable static Bearer-token auth for the **Web UI / non-MCP** endpoints (and the API-Token set the plain transport validates against). No longer a global MCP gate — MCP auth is decided per transport above. When false, those endpoints need no token and all WS origins are accepted. |
| `server.auth.tokens` | list of strings | no | [] | Accepted bearer tokens (constant-time compared); also the API-Token set for `server.mcp.plain.bearer_auth`. |
| `server.auth.allowed_origins` | list of strings | no | [] | When auth is on, permitted WebSocket `Origin` values for `/ws/ui` and `/drafts` (empty Origin rejected). |
| `server.mcp.oauth.{consent_credential, trusted_client_metadata_domains, access_token_ttl, refresh_token_ttl, authorization_code_ttl}` | block | no | off | Built-in OAuth 2.1 authorization server, active when `server.mcp.oauth.listen` is set. See *Enabling OAuth* below. |

† **At least one** of `server.mcp.plain.listen` / `server.mcp.oauth.listen` must be
set (both is valid; neither is a startup error). There are **no `tls` fields** on
either MCP transport — Shoka terminates no TLS (see *TLS*).
| `server.log.level` / `server.log.format` | string | no | `info` / `text` | Structured logging — see *Logging* below. |
| `server.log.output` (+ `server.log.file.*`) | string | no | `stderr` | Where log output goes: `stderr` (default) or a bounded `file`. See *Logging* below. |
| `server.debug.dump_http` | bool | no | `false` | Operator-only verbatim HTTP request/response dump for connect/debug sessions. See *Verbatim HTTP dump* below. |

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
`server.http.listen`, or at least one MCP transport (`server.mcp.plain.listen` /
`server.mcp.oauth.listen`), and rejects an invalid `server.log.level`/`format` or
a `wal_worker` min/max inversion. (Source: `internal/config/config.go`.)

### Strict config decoding (unknown / misplaced keys fail loudly)

The config is decoded **strictly**: an **unknown key** (a typo like `storagee:`) or a
**known key in the wrong block** (e.g. `dump_http:` placed directly under `server:`
instead of under `server.debug:`) is a **hard load error** that names the offending key
and its line — the server does **not** start. Before this, such keys were silently
dropped and took effect nowhere, with no error, discoverable only by restarting and
trial-connecting. Every valid config (including `shoka.example.yaml`) is unaffected.

### Checking a config before restart (`--config-check`)

To confirm a config **without** starting the server or binding any port — the
equivalent of `apachectl configtest` — run:

```sh
shoka --config-check --config /path/to/shoka.yaml
```

It runs the same strict decode + validation the server does at startup and then exits:

- **valid** → exit `0`, prints `config OK` and a terse, address-free summary (which
  transport surfaces would open, their auth posture, and whether the HTTP dump is on —
  category names and booleans only, never an address or secret);
- **invalid** → exit non-zero, prints the exact error to **stderr** (the unknown/misplaced
  key with its line, a bad value, or a failed validation such as the neither-MCP-port
  rule).

Run it before every restart so a misplaced or typo'd key is caught up front instead of
after a failed connect.

## Logging

All subsystems share **one** structured log sink, and `server.log.output`
selects **where that output goes**. The default is **stderr** (unchanged); a
**bounded file** is the other built-in destination. Logs go to stderr — not
stdout — so that under a service manager the journal captures them cleanly; the
MCP transport is Streamable HTTP, so nothing rides stdout.

Under a service manager this is usually all you need: leave `output: stderr` and
let **journald** (systemd) or the launchd `StandardErrorPath` capture and rotate
the stream. The **file** destination is for deployments **not** fronted by a
service manager — it bounds its own on-disk footprint so you don't need an
external rotator (logrotate etc.).

### Configuration

Add a `server.log` block to `shoka.yaml`:

```yaml
server:
  log:
    level: info       # error | warn | info | debug  (default: info)
    format: text      # text | json                  (default: text)
    output: stderr    # stderr (default) | file      — where output goes
    # When output: file, the file is BOUNDED (it never grows forever):
    # file:
    #   path: /var/log/shoka/shoka.log  # required; parent dirs created
    #   max_size_mb: 100   # rotate the active file past this size (0 => 100)
    #   max_backups: 7     # rotated backups kept (0 => 7)
    #   max_age_days: 30   # days rotated backups kept (0 => 30)
    #   compress: false    # gzip rotated backups
    #   rotate_daily: true # also rotate at least once a day (default true)
```

An absent `server.log` block is fully backward-compatible: the server starts at
`info`/`text` on **stderr** without any config change.

| Key | Values | Default | Effect |
|-----|--------|---------|--------|
| `server.log.level` | `error` `warn` `info` `debug` | `info` | Minimum severity to emit. |
| `server.log.format` | `text` `json` | `text` | Human-readable key=value (`text`) or machine-parseable JSON (`json`). Use `json` when shipping logs to a structured collector. |
| `server.log.output` | `stderr` `file` | `stderr` | Where log output goes. `stderr` is the historical default (one shared sink); `file` selects the bounded file destination below. |
| `server.log.file.path` | path | — | Required when `output: file`. Parent directories are created; an unopenable path **fails startup loud** (no silent fall-back to stderr). |
| `server.log.file.max_size_mb` | int ≥ 0 | `100` | Rotate the active file once it exceeds this size. `0` means "use the default", not "unlimited". |
| `server.log.file.max_backups` | int ≥ 0 | `7` | Rotated backups retained (`0` ⇒ default). |
| `server.log.file.max_age_days` | int ≥ 0 | `30` | Days rotated backups are retained (`0` ⇒ default). |
| `server.log.file.compress` | bool | `false` | gzip rotated backups. |
| `server.log.file.rotate_daily` | bool | `true` | Rotate at least once per day even with no size pressure. Rotation by size alone would let a low-traffic log span weeks in one file; the default cycles it daily. Set `false` for size-only. |

The file destination is bounded by [lumberjack](https://github.com/natefinch/lumberjack)
(MIT). lumberjack rotates on **size**; the **daily** trigger is a Shoka addition,
so by default the active file never spans more than a day or `max_size_mb`,
whichever comes first.

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

### Verbatim HTTP dump (`server.debug.dump_http`)

When the redacted protocol traces above are not enough — e.g. a connect fails
*after* the OAuth `/token` step and you need the exact bytes a client sent and
received — enable the verbatim HTTP dump. It is an operator-only diagnostic, **off by
default**, set under `server.debug`:

```yaml
server:
  debug:
    dump_http: true   # default false
```

Restart the server. At startup it prints a one-line state indicator so you can confirm
the running build has it on without a trial connect:

```
INFO startup http dump enabled=true
```

With it on, **every** request and **every** response on **all three** surfaces
(`web`, `mcp-plain`, `mcp-oauth`) is logged in full, at INFO, correlated to the rest
of the trace by `request_id`, as a **guaranteed pair** — no request or response is ever
dropped (boring, secret-bearing, unparseable, error, SSE — all of it):

- `http request dump` — `surface`, `http_method`, `url` (path + full query), all
  `headers`, and the full request `body`.
- `http response dump` — `surface`, `status`, all `headers`, and the full response
  `body`.

**The dump is RAW and UNREDACTED** (B-59). Tokens, authorization codes,
`code_verifier`/`code_challenge`, the consent credential, a `?token=` query value, and
the `Authorization` header value are all dumped **verbatim**, like every other byte —
no masking, no `«redacted»` marker, no fingerprint substitution. The earlier redaction
was removed deliberately: it was dropping the `/token` request dump (the single most
important request in an OAuth connect debug), and for a local, default-OFF switch the
secret values have no protection value that outweighs being able to debug. **Because the
log then contains live secrets, treat it as sensitive: this is a local debug switch you
own — enable it for a debug session, read the log, then turn it back off, and do not
ship the log.**

The request body is buffered and restored before any downstream reader consumes it, so
a request is **never** missing its dump because its body was read by the MCP message
path or the OAuth form parser. SSE/streaming responses are **captured too** (teed as
they flush, so the client's stream is unaffected) — there is no body omission.

Confirm it is working by the `startup http dump enabled=true` line and by an
`http request dump` / `http response dump` pair appearing for your connect.

## TLS

**Shoka terminates no TLS — by design.** It speaks plain HTTP on every listener
and delegates TLS to an external **TLS-terminating reverse proxy** (nginx, etc.)
in front of it. This is deliberate: it keeps the certificate lifecycle —
issuance, renewal, renewal↔reload synchronisation, revocation — in a component
built for it, rather than in Shoka. There are **no `tls` fields on the MCP
transports** (`server.mcp.plain` / `server.mcp.oauth`).

Operational consequences, by transport:

- **OAuth transport** (`server.mcp.oauth`) — OAuth requires HTTPS, so it is always
  reached through the TLS proxy. This is the external entry point.
- **Plain transport with `bearer_auth: true`** (an API-Token) — the token rides in
  the `Authorization` header, so this transport **MUST** sit behind the TLS proxy;
  a cleartext path leaks the token. Treat TLS as a hard requirement here.
- **Plain transport with `bearer_auth: false`** (unauthenticated) — for
  **loopback / internal use only**. Do not expose it beyond the host or a trusted
  network; it has no authentication.

(The proxy, public origin, and addresses are deployment-specific and live only in
the operator's configuration — never in this repository.)

## Enabling OAuth

Shoka includes a built-in **OAuth 2.1 authorization server** (maintenance backlog
B-39), surfaced as the **OAuth MCP transport** (`server.mcp.oauth`). It is
**code-complete but not stood up in this deployment**. The OAuth transport is
**active when `server.mcp.oauth.listen` is set** — there is no separate enable
flag (its presence is the switch). Standing it up is an operator task with
prerequisites, named here by **config field and role only** — no endpoint,
domain, or address is given:

- A **TLS-terminating reverse proxy** in front of Shoka (OAuth requires HTTPS;
  Shoka terminates no TLS — see *TLS*).
- **`server.mcp.oauth.listen`** set — its presence opens the OAuth transport.
- **`server.mcp.oauth.external_url`** set to the public origin — the field; the
  URL value lives only in your config, never in the docs.
- The **`server.mcp.oauth.trusted_client_metadata_domains`** allowlist populated.
  It is **default-deny**: an empty list admits no client, so list the legitimate
  connector domain(s) for your deployment.
- **`server.mcp.oauth.consent_credential`** set — an empty value denies all
  consent, so consent cannot be granted until it is configured.

The OAuth transport speaks **purely OAuth** — static `Authorization: Bearer`
tokens are not accepted on it (mixing a bearer path onto the external port is
forbidden by design). It enforces OAuth access tokens on its `/mcp` path and
identifies clients via CIMD (no dynamic client registration). The separate
plain transport carries the static-bearer / unauthenticated path. For the
protocol detail — discovery, `/authorize`, `/token`, PKCE — see
[`docs/contracts/mcp-v1.md`](contracts/mcp-v1.md) § 3.1. Standing up OAuth means
configuring these fields; it does not point at a running, reachable service.
(Source: `internal/config/config.go`, `internal/oauth/`.)

## Scraping `/metrics`

Shoka can expose a Prometheus **`/metrics`** endpoint. It is **off by default**
and binds **loopback-only**:

- Enable it by setting **`metrics.addr`** (empty = off). A non-empty address is
  forced to a loopback host, mirroring the pprof endpoint.
- It exposes **33 metric families** across five groups — storage/WAL, derivative
  index, lost+found, OAuth, and notify. **No metric label carries a secret,
  token, id, path, or domain**: labels are bounded enums and per-project
  `namespace`/`project` dimensions only. For the family-by-family detail, see
  [`docs/contracts/mcp-v1.md`](contracts/mcp-v1.md) § 7.1 — this guide does not
  re-enumerate it.
- Posture: opt-in, **loopback-only** — scrape locally, or via your own collector
  reaching the loopback bind. "Loopback-only" is a posture, not an address.

(Source: `internal/metrics/`, `cmd/server/main.go:259-274,524-543`.)

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
| Server exits immediately with `... is required` | Missing `storage.base_dir` or `server.http.listen`. (`internal/config/config.go`.) |
| Server exits with `at least one MCP transport must be configured` | Neither `server.mcp.plain.listen` nor `server.mcp.oauth.listen` is set; set at least one. (`internal/config/config.go`.) |
| HTTP **401** on the MCP endpoint | Auth enabled; request lacks a valid `Authorization: Bearer`. Note `?token=` is **not** accepted on the MCP endpoint — header only. |
| HTTP **404** on the MCP endpoint (`request rejected ... status=404` with a `session_id`) | The client presented an `Mcp-Session-Id` this process does not know (normal after a Shoka restart). Expected and self-healing: the client re-initializes. (Contract § 2.) |
| HTTP **403** `invalid Host header` on the MCP endpoint | DNS-rebinding protection: a non-loopback `Host` reached a loopback-bound Shoka (often a reverse proxy forwarding the original `Host`). Fix the proxy `Host`, or start with `MCPGODEBUG=disablelocalhostprotection=1`. (Contract § 2.) |
| WebSocket upgrade **401** | Auth on, no token. Pass the token via `?token=` (allowed on `/ws/ui`, `/drafts/`) or the header. |
| WebSocket upgrade **403** | Auth on and the request `Origin` is not in `allowed_origins` (empty Origin is rejected). |
| `translate_file` tool missing | `services.google_cloud.project_id` is unset, so the tool is not registered. |
| `write_file`/`delete_file` returns a conflict | Another writer changed the file since you read it. Re-read to get the current `etag` (the content-SHA-256 token), pass it as `if_match`, then retry (contract § 5). |
| Webhook never arrives | Check the hook's `events` includes the event, the `url` is reachable, and server logs (delivery is best-effort: 2 attempts, then logged failure). |
| Port already in use on startup | Another process holds `server.http.listen` or an `server.mcp.*.listen`; change the port or stop the other process. |

## Sources

- Source: `internal/config/config.go:11-69` (schema + validation),
  `cmd/server/main.go:29-116,200-216` (startup, listeners, conditional translate,
  auth wiring), `internal/storage/fs_git.go:64-118` (base_dir creation, project
  repos), `.devcontainer/Dockerfile`.
- Documents: `shoka.example.yaml` (annotated config), `docs/contracts/mcp-v1.md`
  (§ 1 versioning, § 3 auth, § 6 webhooks).
