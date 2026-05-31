// Request/response client for the Shoka /ws/ui WebSocket.
//
// Shoka serves all read data (projects, file tree, file content) over /ws/ui as
// request/response messages — there is no REST read API. The protocol has no
// request IDs, but the server's read loop processes one message at a time and
// writes exactly one response per request in order (internal/ui/manager.go), so
// a single FIFO queue of pending requests correlates responses correctly.
//
// Session 1 scope: request/response only. Inbound NOTIFY frames (server push)
// are ignored here; the auto-refresh subscription, reconnect/backoff, and the
// connection-status banner are session 2. On an unexpected close we fail
// in-flight requests (so queries surface an error) rather than reconnecting.

const WS_OPEN = 1

interface Pending {
  resolve: (value: unknown) => void
  reject: (err: Error) => void
}

// Minimal structural type for a WebSocket, so tests can inject a fake.
export interface WsLike {
  send(data: string): void
  close(): void
  readyState: number
  onopen: (() => void) | null
  onclose: (() => void) | null
  onerror: (() => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
}

export type WsFactory = (url: string) => WsLike

export class WsRequestClient {
  private ws: WsLike | null = null
  private connecting = false
  private readonly outbox: string[] = []
  private readonly pending: Pending[] = []

  constructor(
    private readonly url: string,
    private readonly factory: WsFactory = (u) =>
      new WebSocket(u) as unknown as WsLike,
  ) {}

  /** Open the connection eagerly (idempotent). Safe to call before any request. */
  connect(): void {
    if (this.connecting || (this.ws && this.ws.readyState === WS_OPEN)) return
    this.connecting = true
    const ws = this.factory(this.url)
    this.ws = ws
    ws.onopen = () => {
      this.connecting = false
      const queued = this.outbox.splice(0)
      for (const frame of queued) ws.send(frame)
    }
    ws.onmessage = (ev) => this.handleMessage(ev.data)
    ws.onclose = () => {
      this.connecting = false
      this.ws = null
      this.failAll(new Error('WebSocket connection closed'))
    }
    ws.onerror = () => {
      // 'close' follows and drives failAll; nothing to do here.
    }
  }

  /** Send a typed request and resolve with its response payload. */
  request<T>(type: string, payload: unknown): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      this.pending.push({
        resolve: resolve as (v: unknown) => void,
        reject,
      })
      const frame = JSON.stringify({ type, payload })
      if (this.ws && this.ws.readyState === WS_OPEN) {
        this.ws.send(frame)
      } else {
        this.outbox.push(frame)
        this.connect()
      }
    })
  }

  private handleMessage(raw: unknown): void {
    if (typeof raw !== 'string') return
    let msg: { type?: string; payload?: unknown }
    try {
      msg = JSON.parse(raw) as { type?: string; payload?: unknown }
    } catch {
      return
    }
    const type = msg.type
    if (!type) return
    // Session 1 ignores server-pushed NOTIFY frames (auto-refresh is session 2).
    if (type === 'NOTIFY') return

    const entry = this.pending.shift()
    if (!entry) return // stray response with no pending request

    if (type === 'ERROR') {
      const message =
        msg.payload && typeof msg.payload === 'object'
          ? (msg.payload as { message?: string }).message
          : undefined
      entry.reject(new Error(message ?? 'Shoka error'))
      return
    }
    entry.resolve(msg.payload)
  }

  private failAll(err: Error): void {
    this.outbox.splice(0)
    while (this.pending.length > 0) {
      this.pending.shift()!.reject(err)
    }
  }
}

let singleton: WsRequestClient | null = null

function socketUrl(): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${window.location.host}/ws/ui`
}

/** The process-wide /ws/ui client, constructed on first use. */
export function wsClient(): WsRequestClient {
  if (!singleton) singleton = new WsRequestClient(socketUrl())
  return singleton
}
