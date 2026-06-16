import { describe, it, expect } from 'vitest'
import { WsRequestClient, type WsLike } from './wsClient'

// B-28 stage-2: a PERMISSION_DENIED frame surfaces a non-fatal deny toast AND rejects
// the originating request — the app does not crash and the caller is not left hanging.

class FakeWs implements WsLike {
  readyState = 0
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

describe('PERMISSION_DENIED handling', () => {
  it('fires the deny handler and rejects the request', async () => {
    let socket: FakeWs | null = null
    const client = new WsRequestClient('ws://x/ws/ui', {
      factory: () => {
        socket = new FakeWs()
        return socket
      },
    })

    const denied: unknown[] = []
    client.setDenyHandler((p) => denied.push(p))

    client.connect()
    socket!.open()
    const p = client.request('SAVE_FILE', { namespace: 'foo', projectName: 'proj', path: 'x.md', content: 'y' })
    socket!.deliver({
      type: 'PERMISSION_DENIED',
      payload: { op: 'SAVE_FILE', namespace: 'foo', required: 'write', message: 'permission denied: scope does not permit write access to foo' },
    })

    await expect(p).rejects.toThrow(/permission denied/)
    expect(denied).toHaveLength(1)
    expect((denied[0] as { required?: string }).required).toBe('write')
  })
})
