import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, writeFileSync, rmSync, openSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

// E2E harness: build the Shoka binary (which embeds the freshly-built web2
// bundle in server/dist), seed a fixture data dir over /ws/ui, start the
// server, and return a teardown that stops it. The tests then drive the real
// production path: Go static-serve + the /ws/ui request/response surface.

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = join(here, '..', '..', '..') // web2/tests/e2e -> repo root
const PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099)
const MCP_PORT = PORT - 1

interface Rpc {
  type: string
  payload: unknown
}

function rpc(ws: WebSocket, type: string, payload: unknown): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const onMsg = (ev: MessageEvent) => {
      const m = JSON.parse(String(ev.data)) as { type: string; payload?: unknown }
      if (m.type === 'NOTIFY') return
      ws.removeEventListener('message', onMsg)
      if (m.type === 'ERROR') {
        const msg = (m.payload as { message?: string })?.message ?? 'error'
        reject(new Error(msg))
      } else resolve(m.payload)
    }
    ws.addEventListener('message', onMsg)
    ws.send(JSON.stringify({ type, payload } satisfies Rpc))
  })
}

async function waitForHttp(url: string, timeoutMs = 20000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url)
      if (res.ok) return
    } catch {
      // not up yet
    }
    await new Promise((r) => setTimeout(r, 200))
  }
  throw new Error(`server did not become ready at ${url}`)
}

async function seed(wsUrl: string): Promise<void> {
  const ws = new WebSocket(wsUrl)
  await new Promise<void>((resolve, reject) => {
    ws.addEventListener('open', () => resolve())
    ws.addEventListener('error', () => reject(new Error('ws connect failed')))
  })
  // demo/docs — a nested tree, GFM content, a deep file for deep-link tests.
  await rpc(ws, 'CREATE_PROJECT', { namespace: 'demo', projectName: 'docs' })
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'README.md',
    content:
      '# Docs\n\nHello **world**. A table:\n\n| a | b |\n|---|---|\n| 1 | 2 |\n',
  })
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'backlog.md',
    content: '# Backlog\n\n- [ ] one\n- [x] two\n',
  })
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'guides/intro.md',
    content: '# Intro\n\nA nested document for expand-to-active.\n',
  })
  // A recognised code file (yaml): session 4 renders it in a read-only
  // CodeMirror (CodeView), highlighted — not markdown. The "#" line is a yaml
  // comment, never an <h1>.
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'config.yaml',
    content: 'name: docs\nversion: 1\n# not a heading\n',
  })
  // A plain-text file with no recognised language: must still render as a plain
  // <pre>, not markdown and not CodeMirror.
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'notes.txt',
    content: 'plain notes\n# not a heading\n',
  })
  // Markdown with a fenced code block: session 4 highlights fences via
  // rehype-highlight (the .hljs class is the stable hook).
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'code.md',
    content:
      '# Code\n\n```go\nfunc main() {\n\tprintln("hi")\n}\n```\n',
  })
  // A long markdown file so scroll-position restoration is observable.
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'demo',
    projectName: 'docs',
    path: 'long.md',
    content:
      '# Long\n\n' +
      Array.from({ length: 200 }, (_, i) => `Paragraph number ${i} with some text.`).join('\n\n') +
      '\n',
  })
  // team/handbook — a second namespace, for switch-namespace / switch-project.
  await rpc(ws, 'CREATE_PROJECT', { namespace: 'team', projectName: 'handbook' })
  await rpc(ws, 'SAVE_FILE', {
    namespace: 'team',
    projectName: 'handbook',
    path: 'index.md',
    content: '# Handbook\n\nTeam handbook home.\n',
  })
  ws.close()
}

let server: ChildProcess | null = null
let dataDir = ''

export default async function globalSetup(): Promise<() => Promise<void>> {
  const binPath = join(tmpdir(), 'shoka-e2e-bin')
  // Build the binary; it embeds the current server/dist (built by `npm run
  // build` in the test:e2e script before Playwright runs).
  execFileSync('go', ['build', '-o', binPath, './cmd/shoka'], {
    cwd: repoRoot,
    stdio: 'inherit',
  })

  dataDir = mkdtempSync(join(tmpdir(), 'shoka-e2e-data-'))
  const cfgPath = join(dataDir, 'shoka.yaml')
  writeFileSync(
    cfgPath,
    [
      'server:',
      '  http:',
      `    listen: ":${PORT}"`,
      // B-50 schema: the MCP endpoint has two transports selected by config
      // presence — `plain` (unauthenticated here) and `oauth`. The OAuth transport
      // is configured so the server opens the connection store and serves the admin
      // OAUTH_LIST/OAUTH_REVOKE management requests (B-39 (c)); enforcement is
      // MCP-path only, so /ws/ui (the web UI these E2Es drive) is unaffected. A
      // trusted-domain allowlist is given so the store opens (the server only warns
      // on the empty consent credential — we seed the store directly below rather
      // than run a real OAuth flow).
      '  mcp:',
      '    plain:',
      `      listen: ":${MCP_PORT}"`,
      '    oauth:',
      `      listen: ":${MCP_PORT - 1}"`,
      '      trusted_client_metadata_domains:',
      '        - "example.com"',
      // B-71 Stage 2c: a consent credential so the /authorize approval can succeed
      // (the oauth-connect.spec drives the real consent page). Seeded into the
      // example.com "domain" entry's per-domain consent at startup.
      '      consent_credential: "e2e-consent-secret"',
      '  log:',
      '    level: "warn"',
      '  auth:',
      '    enabled: false',
      // B-28 stage 1: these existing E2Es predate the login gate and drive the app
      // directly. Disable the first-run wizard so an empty user store renders the app
      // via the no-lockout single-operator path (the pre-login behaviour). The passkey
      // register+login flow is exercised by its own server in passkey.spec.ts.
      '    users:',
      '      allow_first_run_admin: false',
      'storage:',
      `  base_dir: "${join(dataDir, 'data')}"`,
      '  drift_scan:',
      '    on_startup: true',
      '    interval: 0',
      '',
    ].join('\n'),
  )

  // Seed one OAuth token series BEFORE the server starts (so the bbolt
  // single-writer lock is free). The management view then has a real connection
  // to list and revoke. Test-only fixture tooling using the existing oauthstore
  // public API — no backend/store change. Placeholder client_id only (§0(b)).
  execFileSync('go', ['run', './web/tests/e2e/seed-oauth', join(dataDir, 'data', 'oauth.db')], {
    cwd: repoRoot,
    stdio: 'inherit',
  })

  const logFd = openSync(join(dataDir, 'server.log'), 'w')
  server = spawn(binPath, ['--config', cfgPath], {
    stdio: ['ignore', logFd, logFd],
  })

  await waitForHttp(`http://localhost:${PORT}/`)
  await seed(`ws://localhost:${PORT}/ws/ui`)

  return async () => {
    server?.kill('SIGTERM')
    if (dataDir) rmSync(dataDir, { recursive: true, force: true })
  }
}
