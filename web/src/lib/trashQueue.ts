import { deleteFile, type DeleteResult } from './fileOps'

// The deferred-execution trash queue: the engine behind B-31's Gmail-style
// "trash-can" delete. A delete does NOT execute on the trigger — it ENQUEUES the
// file and starts an n-second client-side countdown. storage.Delete fires (via
// fileOps.deleteFile) ONLY when the countdown elapses, so:
//   - Cancel = clear the timer = the file is never touched = NO write, NO conflict
//     (this is the model's whole safety property: it is not delete-then-restore).
//   - Teardown (provider unmount / page close / navigate away) clears every timer
//     → no deferred delete fires. A reload starts empty: the queue is in-memory
//     React state only, never persisted (no localStorage).
//   - Execution carries the if_match etag captured AT ENQUEUE, so a file edited
//     during the grace comes back as a CONFLICT, not a silent destroy.
//
// It is deliberately framework-light (a plain class, like wsClient): the React
// layer (lib/trashController) wraps it, mapping onChange → state and onExecuted →
// cache relocation / navigation / conflict toast. now/schedule are injectable so
// the deferred-timing contract can be unit-tested deterministically.

export interface TrashItem {
  id: string
  namespace: string
  project: string
  path: string
  // The optimistic-concurrency etag captured at enqueue — the if_match the
  // deferred delete will carry. A mid-grace edit makes it stale → CONFLICT.
  etag: string
  // ms epoch when the deferred delete fires (drives the countdown display).
  deadline: number
}

export interface EnqueueArgs {
  namespace: string
  project: string
  path: string
  etag: string
}

export interface TrashQueueOptions {
  // Grace before the deferred delete fires. Default 10s (directive §0): generous
  // enough that a flustered user cancels calmly.
  graceMs?: number
  // Item-set changed — the React layer re-renders the trash pane + reserved rows.
  onChange?: (items: TrashItem[]) => void
  // A fired delete resolved (ok or conflict) — the React layer relocates caches,
  // follows the deletion, or surfaces a conflict. NEVER called for a cancel.
  onExecuted?: (result: DeleteResult, item: TrashItem) => void
  // Injectable clock + scheduler for deterministic tests (mirrors wsClient).
  now?: () => number
  schedule?: (fn: () => void, ms: number) => () => void
}

const DEFAULT_GRACE_MS = 10_000

export class TrashQueue {
  private items: TrashItem[] = []
  private readonly timers = new Map<string, () => void>() // id -> cancel fn
  private seq = 0

  private readonly graceMs: number
  private readonly onChange?: (items: TrashItem[]) => void
  private readonly onExecuted?: (result: DeleteResult, item: TrashItem) => void
  private readonly now: () => number
  private readonly schedule: (fn: () => void, ms: number) => () => void

  constructor(opts: TrashQueueOptions = {}) {
    this.graceMs = opts.graceMs ?? DEFAULT_GRACE_MS
    this.onChange = opts.onChange
    this.onExecuted = opts.onExecuted
    this.now = opts.now ?? (() => Date.now())
    this.schedule =
      opts.schedule ??
      ((fn, ms) => {
        const t = setTimeout(fn, ms)
        return () => clearTimeout(t)
      })
  }

  /** The current reservations, oldest first. */
  list(): TrashItem[] {
    return this.items
  }

  /**
   * Reserve a file for deletion and start its grace timer. Returns the item id.
   * A file already reserved is NOT re-queued (its existing timer stands), so a
   * double right-click / repeated drag does not schedule two deletes of one path.
   */
  enqueue(args: EnqueueArgs): string {
    const dup = this.items.find(
      (i) =>
        i.namespace === args.namespace &&
        i.project === args.project &&
        i.path === args.path,
    )
    if (dup) return dup.id

    const id = `trash-${this.seq++}`
    const item: TrashItem = { id, ...args, deadline: this.now() + this.graceMs }
    this.items = [...this.items, item]
    this.timers.set(
      id,
      this.schedule(() => this.fire(id), this.graceMs),
    )
    this.emit()
    return id
  }

  /**
   * Cancel a reservation: clear the timer and drop the item. NO delete is sent —
   * the file is never touched. This is the trash-can's safety guarantee.
   */
  cancel(id: string): void {
    this.clearTimer(id)
    if (this.removeItem(id)) this.emit()
  }

  /** Fire the delete immediately (the small/separated "delete now" affordance). */
  executeNow(id: string): void {
    this.clearTimer(id)
    this.fire(id)
  }

  /**
   * Clear every timer without sending anything (provider unmount = teardown). A
   * pending delete simply never fires — close/navigate-away/reload all collapse
   * to "no write", the deferred-execution safety.
   */
  teardown(): void {
    for (const cancel of this.timers.values()) cancel()
    this.timers.clear()
  }

  // The grace elapsed (or "delete now"): drop the item from the queue, then send
  // the delete carrying the enqueue-time etag as if_match. A transport error
  // leaves the item already dropped — a reconnect re-derives state from the tree.
  private fire(id: string): void {
    this.timers.delete(id)
    const item = this.items.find((i) => i.id === id)
    if (!item) return
    this.removeItem(id)
    this.emit()
    void deleteFile({
      namespace: item.namespace,
      project: item.project,
      path: item.path,
      ifMatch: item.etag,
    })
      .then((res) => this.onExecuted?.(res, item))
      .catch(() => {
        /* transport error: item already dropped; reconnect re-derives state */
      })
  }

  private clearTimer(id: string): void {
    const cancel = this.timers.get(id)
    if (cancel) cancel()
    this.timers.delete(id)
  }

  private removeItem(id: string): boolean {
    const before = this.items.length
    this.items = this.items.filter((i) => i.id !== id)
    return this.items.length !== before
  }

  private emit(): void {
    this.onChange?.(this.items)
  }
}
