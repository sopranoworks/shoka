# Shoka

Shoka is a backend server that stores project documentation (milestones, specs,
instructions) as plain Markdown files, versions every change with Git, and exposes
it to coding agents over the **Model Context Protocol (MCP)** while letting humans
edit the same documents through a web UI. It is the authoritative, version-
controlled knowledge base that lets agents operate with high-fidelity instructions
and a full audit trail.

Projects are isolated on the filesystem as `<base_dir>/<namespace>/<project>` —
each its own Git repository. There is no database and there are no UUIDs.

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
    listen: ":8081"       # MCP (SSE) endpoint for agents
```

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
SSE/HTTP) · [go-git](https://github.com/go-git/go-git) (versioning) ·
[gorilla/websocket](https://github.com/gorilla/websocket) (drafts + UI) ·
Google Cloud Translation v3 (optional). See `docs/ARCHITECTURE.md` for why.

## Sources

- `docs/contracts/mcp-v1.md` (interface), `docs/ARCHITECTURE.md` (design),
  `docs/OPERATIONS.md` (config). Quick-start config mirrors `shoka.example.yaml`
  and `internal/config/config.go:58-69` (required fields).
