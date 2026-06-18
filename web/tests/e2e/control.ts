// Test-side control of the real backend: open a direct /ws/ui connection (not
// the page's, so it is unaffected by any client-side WebSocket interception) and
// issue writes, which make the server broadcast NOTIFY to the browser.

const PORT = Number(process.env.SHOKA_E2E_PORT ?? 8099)

function rpc(ws: WebSocket, type: string, payload: unknown): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const onMsg = (ev: MessageEvent) => {
      const m = JSON.parse(String(ev.data)) as { type: string; payload?: unknown }
      if (m.type === 'NOTIFY') return
      ws.removeEventListener('message', onMsg)
      if (m.type === 'ERROR')
        reject(new Error((m.payload as { message?: string })?.message ?? 'error'))
      else resolve(m.payload)
    }
    ws.addEventListener('message', onMsg)
    ws.send(JSON.stringify({ type, payload }))
  })
}

async function withWs<T>(fn: (ws: WebSocket) => Promise<T>): Promise<T> {
  const ws = new WebSocket(`ws://localhost:${PORT}/ws/ui`)
  await new Promise<void>((resolve, reject) => {
    ws.addEventListener('open', () => resolve())
    ws.addEventListener('error', () => reject(new Error('control ws failed')))
  })
  try {
    return await fn(ws)
  } finally {
    ws.close()
  }
}

export function backendWrite(
  namespace: string,
  project: string,
  path: string,
  content: string,
): Promise<void> {
  return withWs(async (ws) => {
    await rpc(ws, 'SAVE_FILE', { namespace, projectName: project, path, content })
  })
}

export function backendCreateProject(
  namespace: string,
  project: string,
): Promise<void> {
  return withWs(async (ws) => {
    await rpc(ws, 'CREATE_PROJECT', { namespace, projectName: project })
  })
}

// Delete a file over the control socket (a git-tracked hard remove), so it lands
// in the project's deleted-file log. Used to seed a deleted file for the
// deleted-view / revive E2E.
export function backendDelete(
  namespace: string,
  project: string,
  path: string,
): Promise<void> {
  return withWs(async (ws) => {
    await rpc(ws, 'DELETE_FILE', { namespace, projectName: project, path })
  })
}

// Move a file over a SEPARATE /ws/ui connection, so the server broadcasts a
// file.move NOTIFY to the page (which is sender-excluded only from its OWN
// connection's moves). Used to test the open-view follow on another connection.
export function backendMove(
  namespace: string,
  project: string,
  sourcePath: string,
  targetPath: string,
): Promise<void> {
  return withWs(async (ws) => {
    await rpc(ws, 'MOVE_FILE', {
      namespace,
      projectName: project,
      source_path: sourcePath,
      target_path: targetPath,
    })
  })
}
