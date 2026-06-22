# Shoka

[![version](https://img.shields.io/badge/version-1.0.0--rc1-blue)](#)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Shoka is a backend server that stores project documentation (milestones, specs,
instructions) as plain Markdown files, versions every change with Git, and exposes
it to coding agents over the **Model Context Protocol (MCP)** while letting humans
edit the same documents through a web UI. It is the authoritative, version-
controlled knowledge base that lets agents operate with high-fidelity instructions
and a full audit trail.

Projects are isolated on the filesystem as `<base_dir>/<namespace>/<project>` —
each its own Git repository. There is no database and there are no UUIDs.

## Features

Beyond basic file CRUD, Shoka provides — see the linked docs for the detail:

- **Rich editing tools** — partial edits (`append_to_file`, `patch_file`) and
  `move_file` alongside read/write/delete. (Contract
  [§ 4](docs/contracts/mcp-v1.md).)
- **Indexed search & link tracking** — full-text search (`search_files`) over a
  project's documents, plus an internal reverse-link index that keeps
  inter-document Markdown links consistent. (Contract § 4.12;
  [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).)
- **OAuth 2.1 authorization server** — a built-in AS (discovery, `/authorize`,
  `/token`) an operator can enable so remote MCP clients connect securely; **off
  by default**. (Contract [§ 3.1](docs/contracts/mcp-v1.md);
  [`docs/OPERATIONS.md`](docs/OPERATIONS.md).)
- **Prometheus `/metrics`** — an opt-in, loopback-only observability endpoint.
  (Contract § 7.1; [`docs/OPERATIONS.md`](docs/OPERATIONS.md).)
- **Self-healing storage** — a lost+found worker restores a tracked-only working
  tree (`shoka.disposable` marks files safe to delete).
  ([`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).)
- **Durable writes** — every change is appended to a write-ahead log and
  committed to Git asynchronously by a background worker pool.
  ([`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).)

## Install

Three ways to install Shoka, in order of preference for a server:

- **Debian/Ubuntu `.deb` — recommended for Linux servers.** Download the package
  for your architecture (`amd64` or `arm64`) from the
  [GitHub Releases](https://github.com/sopranoworks/shoka/releases) page and install it:

  ```sh
  sudo apt install ./shoka_<version>_<arch>.deb
  ```

  This installs the `shoka` server and `shoka-cli` to `/usr/bin`, an
  `/etc/shoka/shoka.yaml` config, a systemd unit, and a `/var/lib/shoka` data
  directory owned by a dedicated `shoka` user (it does **not** auto-start). Then
  edit the config and `sudo systemctl enable --now shoka`. Full walkthrough:
  [`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Installation*).

- **`go install` — any platform with a Go toolchain.** Install both binaries
  directly from the repository:

  ```sh
  go install github.com/sopranoworks/shoka/cmd/shoka@latest
  go install github.com/sopranoworks/shoka/cmd/shoka-cli@latest
  ```

  They land in `~/go/bin` (`$(go env GOBIN)` — put it on your `PATH`). `@latest`
  takes the newest tagged release; pin an exact one with `@vX.Y.Z`. See
  [`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Installation*).

- **Homebrew (macOS) — planned.** A source formula for `brew install` /
  `brew services` is planned; none is published yet. On macOS today, use
  `go install` or build from source (*Quick start* below).

**Supported platforms.** The `.deb` targets currently-supported Debian/Ubuntu
releases (Ubuntu 22.04 + 24.04 LTS, Debian 12 + 13) and their derivatives, on
amd64 and arm64; systemd is required and `adduser` is pulled in automatically by
`apt`. The binaries are statically linked (`CGO_ENABLED=0`), so the build/CI host
OS does not constrain where they run. End-of-support releases (e.g. Ubuntu 20.04,
Debian 11) are outside the supported matrix, and macOS is a separate manual
install. Details: [`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Supported
platforms*).

To build and run from source for development, see *Quick start* below.

## Quick start

Shoka is a Go program. Build it and run it against a config file:

```sh
go build -o shoka ./cmd/shoka
cp shoka.example.yaml shoka.yaml      # then edit as needed
./shoka --config shoka.yaml
```

Minimal config (required fields only):

```yaml
storage:
  base_dir: "./data"      # project repos are created here
server:
  http:
    listen: ":8080"       # web UI + WebSocket endpoints
  mcp:
    plain:                # the plain (internal) MCP transport — served at /mcp
      listen: ":8081"     # MCP (Streamable HTTP) endpoint for agents
```

The MCP surface is configured as up to two transports selected by presence — a
**plain** (internal) one and an **OAuth** (external) one; at least one
`listen` must be set. See *Connecting clients* and the configuration reference in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md).

Point an MCP client at the `/mcp` path on the MCP listener, e.g.:

```sh
claude mcp add --transport http shoka http://localhost:8081/mcp
```

A non-CLI client that cannot register a Streamable-HTTP server directly (e.g.
Claude Desktop) connects through the `mcp-remote` bridge — add an `mcpServers`
entry that runs `npx mcp-remote http://localhost:8081/mcp`. See
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Connecting clients*) for the detail.

**Behind Cloudflare or another CDN/WAF?** If the claude.ai connector fails *after*
OAuth succeeds (`/token` 200, then "Authorization … failed … ofid_…", and no `/mcp`
request reaches the server), the edge bot-defense is dropping Anthropic's cookie-less
server-to-server request — allowlist Anthropic's egress range. See
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Connecting claude.ai behind a CDN / WAF /
bot-defense*).

Authentication is **off** by default (single-operator local mode). See
`shoka.example.yaml` for the full annotated configuration (auth, translation,
webhooks).

**Check a config before restarting.** The config is decoded strictly — an unknown or
misplaced key fails startup loudly, naming the key, instead of being silently ignored.
Run `shoka --config-check --config shoka.yaml` to load + validate it without starting
the server or binding a port: exit `0` and `config OK`, or non-zero with the exact
error. See [`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Strict config decoding*).

**Debugging a connect?** Set `server.debug.dump_http: true` (default off) and
restart — every HTTP request and response on all three surfaces is then logged
**verbatim and unredacted** (method, headers, full body, status — including tokens and
codes in clear), correlated by `request_id`, as a guaranteed `http request dump` /
`http response dump` pair with no exception. The startup line `startup http dump
enabled=true` confirms it is on. The log then contains live secrets — it is a local
debug switch you own: enable it, read it, turn it off, don't ship the log. See
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Verbatim HTTP dump*).

**TLS is outsourced — by design.** Shoka terminates no TLS (it avoids the
certificate lifecycle: issuance, renewal, reload synchronisation, revocation).
Run it behind an external TLS-terminating reverse proxy (nginx, etc.). The plain
transport with `bearer_auth: true` (an API-Token) **must** sit behind that proxy
or the token travels in cleartext; the unauthenticated plain transport is for
loopback/internal use only; the OAuth transport requires HTTPS and so is reached
through the proxy too. See [`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*TLS*).

## Documentation

| Audience | Document |
|----------|----------|
| **MCP client integrators** (the contract) | [`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md) |
| Understanding the design | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Running & configuring | [`docs/OPERATIONS.md`](docs/OPERATIONS.md) |
| **Agents** integrating with Shoka | [`docs/agents/README.md`](docs/agents/README.md) |
| Document conventions | [`docs/conventions/frontmatter.md`](docs/conventions/frontmatter.md), [`document-lifecycle.md`](docs/conventions/document-lifecycle.md), [`failure-records.md`](docs/conventions/failure-records.md) |
| Removing secrets from history | [`docs/operations/sensitive-data-removal.md`](docs/operations/sensitive-data-removal.md) |

The single source of truth for the wire interface is
[`docs/contracts/mcp-v1.md`](docs/contracts/mcp-v1.md). External clients build
against that document.

## Tech stack

Go · [mcp-go-sdk](https://github.com/modelcontextprotocol/go-sdk) (MCP over
Streamable HTTP) · [go-git](https://github.com/go-git/go-git) (versioning) ·
[gorilla/websocket](https://github.com/gorilla/websocket) (drafts + UI) ·
Google Cloud Translation v3 (optional). See `docs/ARCHITECTURE.md` for why.

## Sources

- `docs/contracts/mcp-v1.md` (interface), `docs/ARCHITECTURE.md` (design),
  `docs/OPERATIONS.md` (config). Quick-start config mirrors `shoka.example.yaml`
  and `internal/config/config.go:58-69` (required fields).

## Version

This is **1.0.0-rc1**. The running binary reports it via `shoka --version` (and
`shoka-cli --version`), and the MCP server advertises it in `get_server_info`.

## License

Shoka is licensed under the **MIT License** — see [`LICENSE`](LICENSE) for the
full text. Its dependencies are MIT/Apache-2.0 (permissive, MIT-compatible).
