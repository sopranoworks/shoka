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
  execFileSync('go', ['build', '-o', binPath, './cmd/server'], {
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
      '  mcp:',
      `    listen: ":${MCP_PORT}"`,
      '  log:',
      '    level: "warn"',
      '  auth:',
      '    enabled: false',
      'storage:',
      `  base_dir: "${join(dataDir, 'data')}"`,
      '  drift_scan:',
      '    on_startup: true',
      '    interval: 0',
      '',
    ].join('\n'),
  )

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
