import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

// Mock fileOps.deleteFile so we can assert the deferred-execution contract: the
// delete is sent ONLY on elapse, carrying the enqueue-time etag, and never after
// a cancel/teardown. The factory defers the deref (arrow body) so the spy is read
// at call time, not at module-eval time (mirrors fileOps.test.ts's wsClient mock).
const deleteFileSpy = vi.fn()
vi.mock('./fileOps', () => ({
  deleteFile: (...args: unknown[]) => deleteFileSpy(...args),
}))

import { TrashQueue } from './trashQueue'

describe('TrashQueue — deferred-execution grace', () => {
  beforeEach(() => {
    deleteFileSpy.mockReset()
    deleteFileSpy.mockResolvedValue({ ok: true, path: 'a.md' })
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  // The directive's #2 RED→GREEN core: enqueue must NOT delete; the delete fires
  // only once the grace fully elapses.
  it('#2 CORE: enqueue does not delete; the delete fires only after the grace', async () => {
    const q = new TrashQueue({ graceMs: 10_000 })
    q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    expect(deleteFileSpy).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(9_999)
    expect(deleteFileSpy).not.toHaveBeenCalled() // still within the grace
    await vi.advanceTimersByTimeAsync(1)
    expect(deleteFileSpy).toHaveBeenCalledTimes(1) // grace elapsed → delete
  })

  // The other half of #2: cancel before the grace elapses NEVER deletes — the
  // file is never touched (the trash-can's whole safety property).
  it('#2 CORE: cancel before the grace elapses never deletes', async () => {
    const q = new TrashQueue({ graceMs: 10_000 })
    const id = q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    q.cancel(id)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(deleteFileSpy).not.toHaveBeenCalled()
    expect(q.list()).toHaveLength(0)
  })

  // #3 teardown = cancel: tearing down (provider unmount / navigate away / close)
  // clears pending timers → no deferred delete is ever sent.
  it('#3 teardown clears pending timers — no delete is sent', async () => {
    const q = new TrashQueue({ graceMs: 10_000 })
    q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    q.teardown()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(deleteFileSpy).not.toHaveBeenCalled()
  })

  // #4 etag carried: the fired delete must carry the if_match captured AT ENQUEUE,
  // so a file edited during the grace conflicts instead of being silently destroyed.
  it('#4 the delete carries the etag captured at enqueue (if_match)', async () => {
    const q = new TrashQueue({ graceMs: 5_000 })
    q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'enqueue-etag' })

    await vi.advanceTimersByTimeAsync(5_000)
    expect(deleteFileSpy).toHaveBeenCalledWith(
      expect.objectContaining({
        namespace: 'n',
        project: 'p',
        path: 'a.md',
        ifMatch: 'enqueue-etag',
      }),
    )
  })

  it('dedups: a file already reserved is not queued twice (one timer, one delete)', async () => {
    const q = new TrashQueue({ graceMs: 10_000 })
    const id1 = q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })
    const id2 = q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e2' })

    expect(id2).toBe(id1)
    expect(q.list()).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(deleteFileSpy).toHaveBeenCalledTimes(1)
  })

  it('executeNow fires immediately and does not double-fire when the timer would elapse', async () => {
    const q = new TrashQueue({ graceMs: 10_000 })
    const id = q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    q.executeNow(id)
    await Promise.resolve()
    expect(deleteFileSpy).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(20_000)
    expect(deleteFileSpy).toHaveBeenCalledTimes(1) // the cleared timer did not re-fire
  })

  it('onExecuted receives the resolved result and the fired item', async () => {
    const onExecuted = vi.fn()
    deleteFileSpy.mockResolvedValue({
      ok: false,
      path: 'a.md',
      currentEtag: 'e9',
      message: 'changed',
    })
    const q = new TrashQueue({ graceMs: 1_000, onExecuted })
    q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    await vi.advanceTimersByTimeAsync(1_000)
    expect(onExecuted).toHaveBeenCalledWith(
      expect.objectContaining({ ok: false, currentEtag: 'e9' }),
      expect.objectContaining({ path: 'a.md', etag: 'e1' }),
    )
  })

  it('onChange reflects enqueue (item added) and elapse (item removed)', async () => {
    const onChange = vi.fn()
    const q = new TrashQueue({ graceMs: 1_000, onChange })
    q.enqueue({ namespace: 'n', project: 'p', path: 'a.md', etag: 'e1' })

    expect(onChange).toHaveBeenLastCalledWith([
      expect.objectContaining({ path: 'a.md' }),
    ])
    await vi.advanceTimersByTimeAsync(1_000)
    expect(onChange).toHaveBeenLastCalledWith([])
  })
})
