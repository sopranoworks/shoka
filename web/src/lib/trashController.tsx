import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { readFileFresh, type DeleteResult } from './fileOps'
import { deriveViewContext } from './viewContext'
import { useToast } from './toast'
import { TrashQueue, type TrashItem } from './trashQueue'

// The React layer over the deferred-execution TrashQueue (lib/trashQueue). It is
// the single controller both delete triggers funnel through — tree right-click
// "Delete…" and drag-to-trash (the trash box's react-dnd drop, Shell ShellRail) —
// so no surface sends DELETE_FILE itself; they call enqueuePath, which captures the
// file's CURRENT etag with a fresh read (the if_match the deferred delete will
// carry) and reserves it.
//
// It owns the trash-pane open state and, when a deferred delete actually fires,
// relocates caches and follows the deletion for the issuing client (which is
// sender-excluded from its own file.delete NOTIFY, so it self-refreshes here —
// mirroring moveController.applyMoved). React state only: no localStorage, so a
// reload starts with an empty queue. All hooks are unconditional, top-level,
// fixed order (Rules of Hooks — respect the 1a370a4 #310 fix).

// Default grace before a queued delete fires (directive §0: 10s, configurable via
// the TrashProvider graceMs prop).
export const DEFAULT_TRASH_GRACE_MS = 10_000

export interface EnqueuePathArgs {
  namespace: string
  project: string
  path: string
}

export interface TrashControllerApi {
  items: TrashItem[]
  paneOpen: boolean
  graceMs: number
  togglePane: () => void
  openPane: () => void
  closePane: () => void
  cancel: (id: string) => void
  executeNow: (id: string) => void
  /**
   * Capture the file's current etag, then reserve it (deferred-grace delete). Both
   * delete triggers funnel through this: tree right-click "Delete…" and the trash
   * box's react-dnd drop (Shell ShellRail's useDrop reads the dropped node's path).
   */
  enqueuePath: (args: EnqueuePathArgs) => Promise<void>
}

const Ctx = createContext<TrashControllerApi | null>(null)

export function TrashProvider({
  children,
  graceMs = DEFAULT_TRASH_GRACE_MS,
}: {
  children: ReactNode
  graceMs?: number
}) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { add: addToast } = useToast()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const viewRef = useRef(deriveViewContext(pathname))
  viewRef.current = deriveViewContext(pathname)

  const [items, setItems] = useState<TrashItem[]>([])
  const [paneOpen, setPaneOpen] = useState(false)

  // A fired delete resolved: on success drop the file's caches, refresh the tree,
  // and — if the deleted file is the one on screen — navigate to the project root
  // (the issuer drives its own follow, being excluded from its file.delete
  // NOTIFY). On conflict (edited mid-grace) keep the file and surface a toast.
  const onExecuted = useCallback(
    (res: DeleteResult, item: TrashItem) => {
      if (res.ok) {
        queryClient.removeQueries({
          queryKey: ['file', item.namespace, item.project, item.path],
          exact: true,
        })
        queryClient.invalidateQueries({
          queryKey: ['tree', item.namespace, item.project],
        })
        const v = viewRef.current
        const onDeleted =
          (v.route === 'blob' || v.route === 'edit') &&
          v.namespace === item.namespace &&
          v.project === item.project &&
          (v.path ?? '') === item.path
        if (onDeleted) {
          void navigate({
            to: '/p/$namespace/$project',
            params: { namespace: item.namespace, project: item.project },
          })
        }
      } else {
        addToast({
          level: 'warn',
          text: `Could not delete ${item.path}: it changed after it was queued.`,
        })
        queryClient.invalidateQueries({
          queryKey: ['tree', item.namespace, item.project],
        })
      }
    },
    [queryClient, navigate, addToast],
  )
  const onExecutedRef = useRef(onExecuted)
  onExecutedRef.current = onExecuted

  // The queue is created ONCE: onChange drives React state; onExecuted is read via
  // a ref so the latest cache/nav closure runs without recreating the queue (and
  // restarting timers). graceMs is fixed at first mount (it is configuration).
  const queueRef = useRef<TrashQueue | null>(null)
  if (queueRef.current === null) {
    queueRef.current = new TrashQueue({
      graceMs,
      onChange: setItems,
      onExecuted: (res, item) => onExecutedRef.current(res, item),
    })
  }
  const queue = queueRef.current

  // Teardown clears every pending timer → no deferred delete fires on unmount.
  useEffect(() => () => queue.teardown(), [queue])

  // Auto-collapse rule (B-31 fix H): the pane auto-OPENS on enqueue (below); here it
  // auto-COLLAPSES the moment the queue transitions to empty (every item cancelled or
  // elapsed), so a right-click Delete… never strands an empty pane open. The effect
  // fires only on the items→0 transition (keyed on items.length), so a manually-
  // opened EMPTY pane stays open, and while items remain the manual toggle is fully
  // respected (no auto-close).
  useEffect(() => {
    if (items.length === 0) setPaneOpen(false)
  }, [items.length])

  const enqueuePath = useCallback(
    async ({ namespace, project, path }: EnqueuePathArgs) => {
      try {
        // Capture the CURRENT etag (the if_match the deferred delete will use):
        // a mid-grace edit then makes it stale → CONFLICT, never a silent destroy.
        const f = await readFileFresh(namespace, project, path)
        queue.enqueue({ namespace, project, path, etag: f.etag })
        setPaneOpen(true)
      } catch {
        addToast({ level: 'warn', text: `Could not queue ${path} for deletion.` })
      }
    },
    [queue, addToast],
  )

  const cancel = useCallback((id: string) => queue.cancel(id), [queue])
  const executeNow = useCallback((id: string) => queue.executeNow(id), [queue])
  const togglePane = useCallback(() => setPaneOpen((o) => !o), [])
  const openPane = useCallback(() => setPaneOpen(true), [])
  const closePane = useCallback(() => setPaneOpen(false), [])

  const api = useMemo<TrashControllerApi>(
    () => ({
      items,
      paneOpen,
      graceMs,
      togglePane,
      openPane,
      closePane,
      cancel,
      executeNow,
      enqueuePath,
    }),
    [
      items,
      paneOpen,
      graceMs,
      togglePane,
      openPane,
      closePane,
      cancel,
      executeNow,
      enqueuePath,
    ],
  )

  // The pane itself is NOT rendered here: it is mounted as an in-column collapsible
  // section at the bottom of the sidebar column (Shell → SidebarTrash), so it splits
  // the sidebar vertically instead of floating over it (B-31 fix G).
  return <Ctx.Provider value={api}>{children}</Ctx.Provider>
}

export function useTrashController(): TrashControllerApi {
  const ctx = useContext(Ctx)
  if (!ctx)
    throw new Error('useTrashController must be used within TrashProvider')
  return ctx
}
