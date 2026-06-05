# Shoka

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

## Quick start

Shoka is a Go program. Build it and run it against a config file:

```sh
go build -o shoka ./cmd/server
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
    listen: ":8081"       # MCP (Streamable HTTP) endpoint for agents, served at /mcp
```

Point an MCP client at the `/mcp` path on the MCP listener, e.g.:

```sh
claude mcp add --transport http shoka http://localhost:8081/mcp
```

A non-CLI client that cannot register a Streamable-HTTP server directly (e.g.
Claude Desktop) connects through the `mcp-remote` bridge — add an `mcpServers`
entry that runs `npx mcp-remote http://localhost:8081/mcp`. See
[`docs/OPERATIONS.md`](docs/OPERATIONS.md) (*Connecting clients*) for the detail.

Authentication is **off** by default (single-operator local mode). See
`shoka.example.yaml` for the full annotated configuration (auth, TLS, translation,
webhooks).

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
