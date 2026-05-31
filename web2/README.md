# Shoka frontend prototype — Candidate A (React)

A **throwaway** prototype of the Shoka web UI, built to evaluate a React stack by
operating it in a browser. It renders bundled mock data (no backend).

## Stack

React 19 · TanStack Router v1 (typed search params + splat routes) ·
TanStack Query · cmdk (command palette) · CodeMirror 6 via
`@uiw/react-codemirror` · react-arborist (file tree) · react-markdown +
remark-gfm · react-resizable-panels · Vite + TypeScript.
Design tokens are CSS custom properties on `:root` (dark default + light toggle,
persisted to `localStorage`).

## Run it

```bash
cd tmp/shoka-prototypes/react
npm install
npm run dev
```

Dev server: **http://localhost:5173** (Vite default).

### Keybindings (command palette)

| Shortcut            | Action                                  |
| ------------------- | --------------------------------------- |
| `⌘K` / `Ctrl+K`     | Open command palette                    |
| `⇧⌘P` / `Ctrl+Shift+P` | Open command palette                 |
| `⌘P` / `Ctrl+P`     | Quick-open file (fuzzy, current project)|
| `⌘E` / `Ctrl+E`     | Open current file in the editor         |
| `⌘⇧L` / `Ctrl+Shift+L` | Toggle theme                         |
| `⌘⇧C` / `Ctrl+Shift+C` | Copy deep-link to current view       |
| `↑ ↓ / Enter / Esc` | Navigate / select / back-or-close       |

Palette commands: Go to File, Open Current File in Editor, Switch Project,
Go to Repositories, Toggle Theme, Copy Deep-link. Each shows its keybinding
inline (VS Code style).

### Theme toggle

Bottom-right of the status bar, or `⌘⇧L`, or the palette. Choice persists in
`localStorage` under `shoka-proto-theme`. Dark is the default.

### Routes

- `/` — repository list. Namespace filter is a typed search param: `/?ns=shoka`.
- `/p/$namespace/$project` — project view (tree in sidebar, welcome content).
- `/p/$namespace/$project/blob/$path` — rendered markdown (splat `$path`).
- `/p/$namespace/$project/edit/$path` — CodeMirror editor (Save → `alert("mock save")`).

## §2.3 backend-attach feasibility checks

### 1. Production build → Go-embeddable bundle

```bash
npm run build          # tsc -b && vite build
du -sh dist            # total size
find dist -type f      # exact file layout
```

`vite.config.ts` sets `base: './'`, so `dist/index.html` references assets with
**relative** paths (`./assets/...`). That lets a Go server embed `dist/` and serve
it under any URL prefix without rewriting.

### 2. Static-serve (NOT the dev server)

```bash
npm run serve:dist     # == npx serve -s dist -l 4173
# then: open http://localhost:4173
# verify assets resolve:
curl -s http://localhost:4173/ | head
curl -sI http://localhost:4173/assets/<the-hashed-js-file>.js
```

`-s` makes `serve` do SPA fallback (serve `index.html` for unknown non-asset
paths). Any trivial static server works for the asset-resolution part; the `-s`
flag is what supplies the deep-link fallback in check 3.

### 3. SPA routing fallback

TanStack Router runs in browser **history mode**: the client reads
`window.location` and renders the matching route. The host only needs to serve
`index.html` for non-asset paths — no server-side URL inspection. Verify with
`npx serve -s dist` (which does exactly that fallback) and paste these deep URLs
into a fresh tab / reload them:

- `http://localhost:4173/p/shoka/maintenance` (project route)
- `http://localhost:4173/p/shoka/maintenance/blob/backlog.md` (blob splat)
- `http://localhost:4173/p/shoka/maintenance/edit/backlog.md` (edit splat)

(Substitute real namespace/project/file names from `src/data/mock-data.json`.)
"Serve index.html for any non-asset path" is sufficient.

### 4. WebSocket integration shape

`src/lib/ws.ts` — a typed `WsClient` instantiated at provider level in
`src/main.tsx` via `startWsClient(...)`, **outside** the React tree. It opens
`ws://localhost:9999/mock` (a dead port on purpose — it won't connect), parses
`onmessage` JSON `{type, payload}` envelopes, routes by `type` into
`queryClient.invalidateQueries(...)`, and backoff-reconnects on close. The status
bar shows a static amber "mock WS" indicator.

## What's deliberately omitted

Real backend, real save, `/ws/ui`, full-text search backend (filename quick-open
only), history/blame, multi-file tabs, tests, bundle optimization.

## Layout

```
src/
  main.tsx                 providers + WS bootstrap
  router.tsx               code-based typed route tree
  data/mock-data.json      bundled mock data (imported, never fetched)
  lib/                     data, queries, fuzzy, theme, palette, ws, types
  components/              Shell, TitleBar, ActivityRail, Sidebar, FileTree,
                           StatusBar, CommandPalette, Markdown
  pages/                   RepoList, Project, Blob, Edit
  styles/global.css        design tokens (:root) + base styles
```
