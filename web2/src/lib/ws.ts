import type { QueryClient } from '@tanstack/react-query'

/**
 * Thin typed WebSocket client (feasibility check §2.3.4).
 *
 * Lives OUTSIDE the React tree: it is handed a QueryClient and drives
 * `invalidateQueries` directly when the (mock) Shoka /ws/ui surface pushes
 * NOTIFY envelopes. In the real app this mirrors Shoka's notification center
 * multiplexed onto /ws/ui. Here it points at a dead port so it never actually
 * connects — by design — but it type-checks, reconnects with backoff, and the
 * routing table shows exactly where real events would map onto query keys.
 */

// Envelope shape: { type, payload }. Mirrors Shoka's NOTIFY messages.
export interface WsEnvelope {
  type: string
  payload?: unknown
}

export interface WsPayloads {
  // A file changed under a project -> invalidate that project's queries.
  'file.changed': { namespace: string; project: string; path: string }
  // The project list changed -> invalidate the repo list.
  'projects.changed': Record<string, never>
  // Connection heartbeat (no query impact).
  ping: Record<string, never>
}

export type WsStatus = 'connecting' | 'open' | 'closed'

export interface WsClientOptions {
  url: string
  queryClient: QueryClient
  onStatus?: (status: WsStatus) => void
}

export class WsClient {
  private ws: WebSocket | null = null
  private closedByUser = false
  private attempt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private readonly url: string
  private readonly qc: QueryClient
  private readonly onStatus?: (status: WsStatus) => void

  constructor(opts: WsClientOptions) {
    this.url = opts.url
    this.qc = opts.queryClient
    this.onStatus = opts.onStatus
  }

  connect(): void {
    this.closedByUser = false
    this.open()
  }

  private open(): void {
    this.onStatus?.('connecting')
    let socket: WebSocket
    try {
      socket = new WebSocket(this.url)
    } catch {
      // Bad URL / blocked: schedule a retry rather than throwing.
      this.scheduleReconnect()
      return
    }
    this.ws = socket

    socket.addEventListener('open', () => {
      this.attempt = 0
      this.onStatus?.('open')
    })

    socket.addEventListener('message', (ev) => {
      this.handleMessage(ev.data)
    })

    socket.addEventListener('close', () => {
      this.onStatus?.('closed')
      if (!this.closedByUser) this.scheduleReconnect()
    })

    // 'error' is followed by 'close'; swallow to avoid console noise on the
    // expected dead-port failure.
    socket.addEventListener('error', () => {})
  }

  private handleMessage(raw: unknown): void {
    if (typeof raw !== 'string') return
    let env: WsEnvelope
    try {
      env = JSON.parse(raw) as WsEnvelope
    } catch {
      return
    }
    this.route(env)
  }

  // Route by envelope.type into TanStack Query cache invalidations.
  private route(env: WsEnvelope): void {
    switch (env.type) {
      case 'file.changed': {
        const p = env.payload as WsPayloads['file.changed']
        if (!p) return
        void this.qc.invalidateQueries({
          queryKey: ['project', p.namespace, p.project],
        })
        void this.qc.invalidateQueries({
          queryKey: ['file', p.namespace, p.project, p.path],
        })
        break
      }
      case 'projects.changed': {
        void this.qc.invalidateQueries({ queryKey: ['projects'] })
        break
      }
      case 'ping':
        break
      default:
        // Unknown type: ignore (forward-compatible).
        break
    }
  }

  private scheduleReconnect(): void {
    if (this.closedByUser) return
    this.attempt += 1
    // Exponential backoff capped at 30s with jitter.
    const base = Math.min(30_000, 500 * 2 ** Math.min(this.attempt, 6))
    const delay = base + Math.random() * 250
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer)
    this.reconnectTimer = setTimeout(() => this.open(), delay)
  }

  close(): void {
    this.closedByUser = true
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer)
    this.ws?.close()
    this.ws = null
  }
}

let singleton: WsClient | null = null

// Instantiated once at provider level (see main.tsx). Idempotent.
export function startWsClient(opts: WsClientOptions): WsClient {
  if (singleton) return singleton
  singleton = new WsClient(opts)
  singleton.connect()
  return singleton
}
