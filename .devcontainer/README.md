# Shoka Devcontainer

Provides a Go toolchain that matches `go.mod` (Go 1.26+) for building and testing Shoka.

## What you get
- Go via `mcr.microsoft.com/devcontainers/go:1-bookworm` (latest Go 1.x, which satisfies
  the `go 1.26.2` directive in `go.mod`; Go's automatic toolchain management fetches the
  exact patch release if the base image lags).
- Ports **8080** (web UI) and **8081** (MCP / SSE) forwarded to the host.

## Verify the toolchain
On container creation, all three of these should pass:

```bash
go build ./...
go vet ./...
go test ./...
```

The embedded frontend (`server/dist`) is committed, so `go build` works without Node.js.

## Run the server
A ready-to-use config ships at the repo root as `shoka.example.yaml`:

```bash
go build -o shoka ./cmd/shoka
./shoka --config shoka.example.yaml
```

This listens on `:8080` (web editor — open http://localhost:8080) and `:8081` (MCP/SSE
endpoint for an MCP client), storing project data under `./data` (created automatically).

To use your own configuration, copy and edit it:

```bash
cp shoka.example.yaml shoka.yaml   # then edit
./shoka --config shoka.yaml
```

## Notes
- Node.js is intentionally **not** installed: it is only needed to rebuild the web
  frontend, and the built assets are already committed under `server/dist`.
