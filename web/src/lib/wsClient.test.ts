import { describe, it, expect, beforeEach } from 'vitest'
import { WsRequestClient, backoffBaseMs, type WsLike } from './wsClient'

// A controllable fake WebSocket implementing the structural WsLike type.
class FakeWs implements WsLike {
  readyState = 0 // CONNECTING
  sent: string[] = []
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((ev: { data: unknown }) => void) | null = null

  send(data: string): void {
    this.sent.push(data)
  }
  close(): void {
    this.readyState = 3
    this.onclose?.()
  }
  open(): void {
    this.readyState = 1
    this.onopen?.()
  }
  deliver(obj: unknown): void {
    this.onmessage?.({ data: JSON.stringify(obj) })
  }
}

// A manual scheduler: captures scheduled callbacks so tests fire them on demand.
function makeScheduler() {
  const queue: { fn: () => void; ms: number }[] = []
  const schedule = (fn: () => void, ms: number) => {
    const entry = { fn, ms }
    queue.push(entry)
    return () => {
      const i = queue.indexOf(entry)
      if (i >= 0) queue.splice(i, 1)
    }
  }
  return {
    schedule,
    nextDelay: () => queue[0]?.ms,
    fireNext: () => queue.shift()?.fn(),
  }
}

let sockets: FakeWs[]
const factory = () => {
  const f = new FakeWs()
  sockets.push(f)
  return f
}
const current = () => sockets[sockets.length - 1]

beforeEach(() => {
  sockets = []
})

describe('backoffBaseMs', () => {
  it('follows 0, 1s, 2s, 4s, 8s, 16s, then caps at 30s', () => {
    expect([0, 1, 2, 3, 4, 5, 6, 7].map(backoffBaseMs)).toEqual([
      0, 1000, 2000, 4000, 8000, 16000, 30000, 30000,
    ])
  })
})

describe('WsRequestClient request/response', () => {
  it('buffers a request made before open, then flushes on open', async () => {
    const c = new WsRequestClient('ws://t/ws/ui', { factory })
    const p = c.request<number[]>('GET_PROJECTS', {})
    expect(current().sent).toEqual([])
    current().open()
    expect(JSON.parse(current().sent[0])).toEqual({
      type: 'GET_PROJECTS',
      payload: {},
    })
    current().deliver({ type: 'GET_PROJECTS', payload: [1, 2, 3] })
    await expect(p).resolves.toEqual([1, 2, 3])
  })

  it('correlates concurrent responses in FIFO order', async () => {
    const c = new WsRequestClient('ws://t/ws/ui', { factory })
    c.connect()
    current().open()
    const a = c.request<string>('GET_TREE', { project: 'a' })
    const b = c.request<string>('GET_TREE', { project: 'b' })
    current().deliver({ type: 'GET_TREE', payload: 'first' })
    current().deliver({ type: 'GET_TREE', payload: 'second' })
    await expect(a).resolves.toBe('first')
    await expect(b).resolves.toBe('second')
  })

  it('rejects on an ERROR response', async () => {
    const c = new WsRequestClient('ws://t/ws/ui', { factory })
    c.connect()
    current().open()
    const p = c.request('READ_FILE', { path: 'missing' })
    current().deliver({ type: 'ERROR', payload: { message: 'file not found' } })
    await expect(p).rejects.toThrow('file not found')
  })
})

describe('WsRequestClient NOTIFY dispatch', () => {
  it('routes NOTIFY to the handler without consuming a pending request', async () => {
    const c = new WsRequestClient('ws://t/ws/ui', { factory })
    c.connect()
    current().open()
    const notified: unknown[] = []
    c.setNotifyHandler((p) => notified.push(p))
    const p = c.request<string>('READ_FILE', { path: 'x' })
    current().deliver({ type: 'NOTIFY', payload: { kind: 'file.write' } })
    current().deliver({ type: 'READ_FILE', payload: 'content' })
    await expect(p).resolves.toBe('content')
    expect(notified).toEqual([{ kind: 'file.write' }])
  })
})

describe('WsRequestClient reconnect', () => {
  it('reports connecting -> connected on open', () => {
    const c = new WsRequestClient('ws://t/ws/ui', { factory })
    const states: string[] = []
    c.subscribeState((s) => states.push(s.status))
    c.connect()
    current().open()
    expect(c.getState().status).toBe('connected')
    expect(states).toContain('connecting')
    expect(states).toContain('connected')
  })

  it('schedules a reconnect with growing backoff', () => {
    const sched = makeScheduler()
    const c = new WsRequestClient('ws://t/ws/ui', {
      factory,
      schedule: sched.schedule,
      now: () => 0,
      random: () => 0.5, // jitter factor 0.75 + 0.5*0.5 = 1.0 -> delay == base
    })
    c.connect()
    current().open() // connected, attempt reset to 0
    current().close() // 1st close: base 0 -> immediate retry
    expect(c.getState().status).toBe('reconnecting')
    expect(sched.nextDelay()).toBe(0)
    sched.fireNext() // opens socket 2 (connecting), no open()
    current().close() // 2nd close: attempt 1 -> 1000ms
    expect(sched.nextDelay()).toBe(1000)
    sched.fireNext()
    current().close() // attempt 2 -> 2000ms
    expect(sched.nextDelay()).toBe(2000)
  })

  it('enters disconnected once backoff reaches the cap', () => {
    const sched = makeScheduler()
    const c = new WsRequestClient('ws://t/ws/ui', {
      factory,
      schedule: sched.schedule,
      now: () => 0,
      random: () => 0.5,
    })
    c.connect()
    current().open()
    // Ramp the attempt counter to 6 (close+retry six times).
    for (let i = 0; i < 6; i++) {
      current().close()
      sched.fireNext()
    }
    // The next close schedules with base at the 30s cap -> disconnected.
    current().close()
    expect(c.getState().status).toBe('disconnected')
  })

  it('rejects in-flight requests on close but queues new ones for reconnect', async () => {
    const sched = makeScheduler()
    const c = new WsRequestClient('ws://t/ws/ui', {
      factory,
      schedule: sched.schedule,
      now: () => 0,
      random: () => 0.5,
    })
    c.connect()
    current().open()
    const inFlight = c.request('GET_PROJECTS', {})
    current().close()
    await expect(inFlight).rejects.toThrow(/closed/)
    const before = sockets.length
    const queued = c.request<string>('GET_TREE', { p: 1 })
    expect(sockets.length).toBe(before) // waits for the scheduled reconnect
    sched.fireNext()
    current().open() // flushes outbox
    expect(current().sent.length).toBe(1)
    current().deliver({ type: 'GET_TREE', payload: 'ok' })
    await expect(queued).resolves.toBe('ok')
  })

  it('reconnectNow resets backoff and reconnects immediately', () => {
    const sched = makeScheduler()
    const c = new WsRequestClient('ws://t/ws/ui', {
      factory,
      schedule: sched.schedule,
      now: () => 0,
      random: () => 0.5,
    })
    c.connect()
    current().open()
    current().close()
    sched.fireNext()
    current().close()
    expect(c.getState().attempt).toBeGreaterThan(1)
    c.reconnectNow()
    expect(c.getState().attempt).toBe(0)
    current().open()
    expect(c.getState().status).toBe('connected')
  })
})
