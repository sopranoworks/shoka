# Shoka Web UI

The Shoka web UI: a client-only React SPA, served as a static bundle embedded in
the Go server, that reads project documentation over the `/ws/ui` WebSocket.

> **Status:** session 1 of the rebuild — a viewing-only experience (browse +
> read) at VS Code Web calibre. Live auto-refresh (session 2), the editor
> (session 3), and full-text search (session 4) land in later sessions. Until
> the editor ships, file editing happens via the Shoka MCP `write_file` tool.

## Stack

React 19 · TanStack Router v1 (typed search params + splat routes) · TanStack
Query · cmdk (command palette) · react-arborist (file tree) · react-markdown +
remark-gfm · react-resizable-panels · Vite + TypeScript. Design tokens are CSS
custom properties on `:root` (dark default + light toggle, persisted to
`localStorage`).

## Develop

The UI talks to a running Shoka binary (default HTTP listen `:8080`). Start
Shoka, then:

```bash
cd web2
npm install
npm run dev      # http://localhost:5173, proxies /ws/ui and /api to :8080
```

`vite.config.ts` proxies `/ws/ui` (WebSocket) and `/api` to `localhost:8080`, so
the dev server hot-reloads the UI while talking to the real backend.

## Build & serve

```bash
npm run build    # tsc -b && vite build  ->  ../server/dist
```

The build writes into `../server/dist`, the directory the Go server embeds
(`server/embed.go`: `//go:embed dist`). Go's embed pattern cannot use `..`, so
the build writes into `server/dist` directly; rebuild the Go binary to embed the
new bundle. The Go server serves the SPA at the domain root and falls back to
`index.html` for unknown non-asset paths.

### Asset base — why `base: '/'`

`vite.config.ts` sets `base: '/'` so `index.html` references assets with
**absolute** paths (`/assets/...`). Shoka serves the UI only at the domain root,
and a deep-link boot (opening e.g. `/p/shoka/maintenance/blob/backlog.md`
directly) must resolve its JS/CSS from the root, not against the deep URL. The
prototype's `base: './'` produced relative paths that 404 at depth — the v3 §0
correction this build applies.

## Test

```bash
npm run test        # Vitest + React Testing Library (unit/component)
npm run test:e2e    # builds the bundle, then Playwright against a real binary
```

`test:e2e` builds the Shoka binary (embedding the bundle), seeds a fixture data
dir over `/ws/ui`, starts the server, and runs the calibre smoke suite
(URL-as-state, deep-link boot, command palette, quick-open, theme persistence,
responsive reflow). See `tests/e2e/global-setup.ts`. CI wiring is a later,
separate task.

## Keybindings

| Shortcut               | Action                                   |
| ---------------------- | ---------------------------------------- |
| `⌘K` / `⇧⌘P`           | Open command palette                     |
| `⌘P`                   | Quick-open file (fuzzy, across projects) |
| `⌘⇧L`                  | Toggle theme                             |
| `⌘⇧C`                  | Copy deep-link to current view           |
| `↑ ↓ / Enter / Esc`    | Navigate / select / back-or-close        |

Palette commands: Go to File, Switch Project, Switch Namespace, Go Home, Toggle
Theme, Copy Deep-link — each shows its keybinding inline (VS Code style).

## Routes

- `/` — repository list. Namespace filter is a typed search param: `/?ns=shoka`.
- `/p/$namespace/$project` — project view (tree in the sidebar, welcome content).
- `/p/$namespace/$project/blob/$path` — rendered markdown / plain text (splat).

The `/edit/$path` route lands in session 3.

## Data flow

All reads go over `/ws/ui` (`lib/wsClient.ts`) as request/response messages
(`GET_PROJECTS`, `GET_TREE`, `READ_FILE`) — Shoka has no REST read API. The
client correlates responses to requests by FIFO order (the server answers one
response per request, in order). Responses flow through TanStack Query, keyed
`['projects']`, `['tree', ns, project]`, `['file', ns, project, path]`, so
navigation is cache-driven and instant on revisit. Inbound `NOTIFY` frames are
ignored in session 1; auto-refresh on `NOTIFY` is session 2.

## Layout

```
src/
  main.tsx                 providers + /ws/ui connect
  router.tsx               code-based typed route tree
  lib/                     wsClient, queries, tree, fuzzy, theme, palette, types
  components/              Shell, TitleBar, ActivityRail, Sidebar, FileTree,
                           StatusBar, CommandPalette, Markdown
  pages/                   RepoList, Project, Blob
  styles/global.css        design tokens (:root) + base styles
  test/setup.ts            Vitest + jest-dom setup
tests/e2e/                 Playwright harness + calibre smoke suite
```
