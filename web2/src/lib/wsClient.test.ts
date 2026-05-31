import { describe, it, expect, beforeEach } from 'vitest'
import { WsRequestClient, type WsLike } from './wsClient'

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

  // test controls
  open(): void {
    this.readyState = 1
    this.onopen?.()
  }
  deliver(obj: unknown): void {
    this.onmessage?.({ data: JSON.stringify(obj) })
  }
}

let fake: FakeWs
let client: WsRequestClient

beforeEach(() => {
  fake = new FakeWs()
  client = new WsRequestClient('ws://test/ws/ui', () => fake)
})

describe('WsRequestClient', () => {
  it('buffers a request made before open, then flushes on open', async () => {
    const p = client.request<number[]>('GET_PROJECTS', {})
    expect(fake.sent).toEqual([]) // not open yet, buffered
    fake.open()
    expect(JSON.parse(fake.sent[0])).toEqual({ type: 'GET_PROJECTS', payload: {} })
    fake.deliver({ type: 'GET_PROJECTS', payload: [1, 2, 3] })
    await expect(p).resolves.toEqual([1, 2, 3])
  })

  it('correlates concurrent responses in FIFO order', async () => {
    fake.open()
    const a = client.request<string>('GET_TREE', { project: 'a' })
    const b = client.request<string>('GET_TREE', { project: 'b' })
    // Responses arrive in request order (the server is strictly sequential).
    fake.deliver({ type: 'GET_TREE', payload: 'first' })
    fake.deliver({ type: 'GET_TREE', payload: 'second' })
    await expect(a).resolves.toBe('first')
    await expect(b).resolves.toBe('second')
  })

  it('ignores NOTIFY frames without consuming a pending request', async () => {
    fake.open()
    const p = client.request<string>('READ_FILE', { path: 'x' })
    fake.deliver({ type: 'NOTIFY', payload: { kind: 'file.write' } })
    fake.deliver({ type: 'READ_FILE', payload: 'content' })
    await expect(p).resolves.toBe('content')
  })

  it('rejects on an ERROR response with the server message', async () => {
    fake.open()
    const p = client.request('READ_FILE', { path: 'missing' })
    fake.deliver({ type: 'ERROR', payload: { message: 'file not found' } })
    await expect(p).rejects.toThrow('file not found')
  })

  it('fails in-flight requests when the socket closes (no reconnect in session 1)', async () => {
    fake.open()
    const p = client.request('GET_PROJECTS', {})
    fake.close()
    await expect(p).rejects.toThrow(/closed/)
  })
})
