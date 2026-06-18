import { describe, it, expect, vi, beforeEach } from 'vitest'

const request = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ request }),
}))

import { listDeleted, reviveFile } from './deletedOps'

describe('listDeleted', () => {
  beforeEach(() => request.mockReset())

  it('requests LIST_DELETED and returns the deleted entries', async () => {
    request.mockResolvedValue({
      namespace: 'ns',
      projectName: 'proj',
      deleted: [
        { path: 'a.md', deletionCommit: 'abc123', deletedAt: '2026-06-18T00:00:00Z' },
      ],
    })
    const out = await listDeleted('ns', 'proj')
    expect(out).toHaveLength(1)
    expect(out[0].path).toBe('a.md')
    expect(request).toHaveBeenCalledWith('LIST_DELETED', {
      namespace: 'ns',
      projectName: 'proj',
    })
  })

  it('normalises a missing deleted field to []', async () => {
    request.mockResolvedValue({ namespace: 'ns', projectName: 'proj' })
    expect(await listDeleted('ns', 'proj')).toEqual([])
  })
})

describe('reviveFile', () => {
  beforeEach(() => request.mockReset())

  it('requests REVIVE_FILE with the path and omits fromCommit when absent', async () => {
    request.mockResolvedValue({ namespace: 'ns', projectName: 'proj', path: 'a.md', revived: true })
    const ack = await reviveFile('ns', 'proj', 'a.md')
    expect(ack.revived).toBe(true)
    expect(request).toHaveBeenCalledWith('REVIVE_FILE', {
      namespace: 'ns',
      projectName: 'proj',
      path: 'a.md',
    })
  })

  it('passes fromCommit when provided', async () => {
    request.mockResolvedValue({ namespace: 'ns', projectName: 'proj', path: 'a.md', revived: true })
    await reviveFile('ns', 'proj', 'a.md', 'deadbeef')
    expect(request).toHaveBeenCalledWith('REVIVE_FILE', {
      namespace: 'ns',
      projectName: 'proj',
      path: 'a.md',
      fromCommit: 'deadbeef',
    })
  })
  // Divergence-error propagation is covered at the wsClient layer (wsClient rejects
  // on an ERROR frame, see wsClient.test.ts) and end-to-end by the Go tests; not
  // re-asserted here to avoid a spurious unhandled-rejection flag in jsdom.
})
