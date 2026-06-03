import { describe, it, expect, vi, beforeEach } from 'vitest'

const requestFrame = vi.fn()
const request = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ requestFrame, request }),
}))

import { saveFile, fileExists, moveFile } from './fileOps'

describe('saveFile', () => {
  beforeEach(() => {
    requestFrame.mockReset()
    request.mockReset()
  })

  it('maps a SAVE_ACK frame to ok:true with the new etag, sending if_match', async () => {
    requestFrame.mockResolvedValue({
      type: 'SAVE_ACK',
      payload: { path: 'a.md', status: 'ok', etag: 'e2' },
    })
    const r = await saveFile({
      namespace: 'n',
      project: 'p',
      path: 'a.md',
      content: 'x',
      ifMatch: 'e1',
    })
    expect(r).toEqual({ ok: true, path: 'a.md', etag: 'e2' })
    expect(requestFrame).toHaveBeenCalledWith(
      'SAVE_FILE',
      expect.objectContaining({ if_match: 'e1', path: 'a.md', content: 'x' }),
    )
  })

  it('maps a CONFLICT frame to ok:false with the current etag (not mistaken for success)', async () => {
    requestFrame.mockResolvedValue({
      type: 'CONFLICT',
      payload: { path: 'a.md', current_etag: 'e9', message: 'modified by someone else' },
    })
    const r = await saveFile({
      namespace: 'n',
      project: 'p',
      path: 'a.md',
      content: 'x',
      ifMatch: 'e1',
    })
    expect(r).toEqual({
      ok: false,
      path: 'a.md',
      currentEtag: 'e9',
      message: 'modified by someone else',
    })
  })

  it('omits if_match for an unchecked create (ifMatch null)', async () => {
    requestFrame.mockResolvedValue({
      type: 'SAVE_ACK',
      payload: { path: 'new.md', status: 'ok', etag: 'e1' },
    })
    await saveFile({
      namespace: 'n',
      project: 'p',
      path: 'new.md',
      content: 'x',
      ifMatch: null,
    })
    const sent = requestFrame.mock.calls[0][1] as Record<string, unknown>
    expect('if_match' in sent).toBe(false)
  })
})

describe('moveFile', () => {
  beforeEach(() => {
    requestFrame.mockReset()
    request.mockReset()
  })

  it('maps MOVE_ACK to ok:true with the new etag, sending source/target paths', async () => {
    requestFrame.mockResolvedValue({
      type: 'MOVE_ACK',
      payload: {
        source_path: 'old.md',
        target_path: 'sub/new.md',
        new_etag: 'e2',
        links_rewritten: 0,
      },
    })
    const r = await moveFile({
      namespace: 'n',
      project: 'p',
      sourcePath: 'old.md',
      targetPath: 'sub/new.md',
    })
    expect(r).toEqual({
      ok: true,
      sourcePath: 'old.md',
      targetPath: 'sub/new.md',
      newEtag: 'e2',
    })
    expect(requestFrame).toHaveBeenCalledWith(
      'MOVE_FILE',
      expect.objectContaining({
        namespace: 'n',
        projectName: 'p',
        source_path: 'old.md',
        target_path: 'sub/new.md',
      }),
    )
  })

  it('omits if_match by default (so a move onto an occupied target is refused, not overwritten)', async () => {
    requestFrame.mockResolvedValue({
      type: 'MOVE_ACK',
      payload: { source_path: 'a.md', target_path: 'b.md', new_etag: 'e1', links_rewritten: 0 },
    })
    await moveFile({ namespace: 'n', project: 'p', sourcePath: 'a.md', targetPath: 'b.md' })
    const sent = requestFrame.mock.calls[0][1] as Record<string, unknown>
    expect('if_match' in sent).toBe(false)
  })

  it('sends if_match when given (explicit overwrite path)', async () => {
    requestFrame.mockResolvedValue({
      type: 'MOVE_ACK',
      payload: { source_path: 'a.md', target_path: 'b.md', new_etag: 'e3', links_rewritten: 0 },
    })
    await moveFile({
      namespace: 'n',
      project: 'p',
      sourcePath: 'a.md',
      targetPath: 'b.md',
      ifMatch: 'targetEtag',
    })
    const sent = requestFrame.mock.calls[0][1] as Record<string, unknown>
    expect(sent.if_match).toBe('targetEtag')
  })

  it('maps CONFLICT to ok:false with the target current etag (occupied target)', async () => {
    requestFrame.mockResolvedValue({
      type: 'CONFLICT',
      payload: { path: 'b.md', current_etag: 'e9', message: 'target exists' },
    })
    const r = await moveFile({
      namespace: 'n',
      project: 'p',
      sourcePath: 'a.md',
      targetPath: 'b.md',
    })
    expect(r).toEqual({
      ok: false,
      path: 'b.md',
      currentEtag: 'e9',
      message: 'target exists',
    })
  })
})

describe('fileExists', () => {
  beforeEach(() => request.mockReset())

  it('reports exists + etag when READ_FILE resolves', async () => {
    request.mockResolvedValue({ path: 'a.md', content: 'x', etag: 'e1' })
    expect(await fileExists('n', 'p', 'a.md')).toEqual({ exists: true, etag: 'e1' })
  })

  it('reports not-exists when READ_FILE rejects (missing file)', async () => {
    const rejected = Promise.reject(new Error('file not found'))
    rejected.catch(() => {}) // mark handled; it still rejects when awaited
    request.mockReturnValueOnce(rejected)
    expect(await fileExists('n', 'p', 'missing.md')).toEqual({ exists: false })
  })
})
