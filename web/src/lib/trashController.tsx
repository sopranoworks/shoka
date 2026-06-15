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
import { getDragSource } from './dragSource'
import { useToast } from './toast'
import { TrashQueue, type TrashItem } from './trashQueue'
import { TrashPane } from '../components/TrashPane'

// The React layer over the deferred-execution TrashQueue (lib/trashQueue). It is
// the single controller both delete triggers funnel through — tree right-click
// "Delete…" and drag-to-trash — so no surface sends DELETE_FILE itself; they call
// enqueuePath (or enqueueFromDrag), which captures the file's CURRENT etag with a
// fresh read (the if_match the deferred delete will carry) and reserves it.
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
  cancel: (id: string) => void
  executeNow: (id: string) => void
  /** Trigger 1 (right-click Delete…): capture the current etag, then reserve. */
  enqueuePath: (args: EnqueuePathArgs) => Promise<void>
  /** Trigger 2 (drag-to-trash): reserve the file recorded at drag-start. */
  enqueueFromDrag: () => void
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

  const enqueueFromDrag = useCallback(() => {
    const src = getDragSource()
    if (!src) return
    void enqueuePath(src)
  }, [enqueuePath])

  const cancel = useCallback((id: string) => queue.cancel(id), [queue])
  const executeNow = useCallback((id: string) => queue.executeNow(id), [queue])
  const togglePane = useCallback(() => setPaneOpen((o) => !o), [])
  const openPane = useCallback(() => setPaneOpen(true), [])

  const api = useMemo<TrashControllerApi>(
    () => ({
      items,
      paneOpen,
      graceMs,
      togglePane,
      openPane,
      cancel,
      executeNow,
      enqueuePath,
      enqueueFromDrag,
    }),
    [
      items,
      paneOpen,
      graceMs,
      togglePane,
      openPane,
      cancel,
      executeNow,
      enqueuePath,
      enqueueFromDrag,
    ],
  )

  return (
    <Ctx.Provider value={api}>
      {children}
      {paneOpen && (
        <TrashPane
          items={items}
          onCancel={cancel}
          onDeleteNow={executeNow}
          onClose={() => setPaneOpen(false)}
        />
      )}
    </Ctx.Provider>
  )
}

export function useTrashController(): TrashControllerApi {
  const ctx = useContext(Ctx)
  if (!ctx)
    throw new Error('useTrashController must be used within TrashProvider')
  return ctx
}
