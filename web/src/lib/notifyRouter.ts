import type { QueryClient } from '@tanstack/react-query'

// Routes /ws/ui NOTIFY events (and reconnect revalidation) into TanStack Query
// cache effects + user-facing banner/toast intents.
//
// Event shape is source-verified from internal/notify/center.go: the four kinds
// that reach the browser are file.write, file.delete, project.create, and
// catalog.invariant_violation. `target` is the JOINED string "namespace/project"
// (not separate fields — the directive §1.2 hint was wrong here); there is no
// project.delete event. See reports/progress/2026-06-01-session-2-notify-event-inventory.md.

export interface NotifyEvent {
  kind: string
  target: string // "namespace/project"
  path?: string
  seq?: number
  timestamp?: string
}

export interface ViewContext {
  route: 'home' | 'project' | 'blob' | 'other'
  namespace?: string
  project?: string
  path?: string
}

export interface BannerIntent {
  text: string
  reload: () => void
}
export interface ToastIntent {
  level: 'warn'
  text: string
}
export interface RouterResult {
  banner?: BannerIntent
  toast?: ToastIntent
}

export function parseNotifyEvent(payload: unknown): NotifyEvent | null {
  if (!payload || typeof payload !== 'object') return null
  const p = payload as Record<string, unknown>
  if (typeof p.kind !== 'string' || typeof p.target !== 'string') return null
  return {
    kind: p.kind,
    target: p.target,
    path: typeof p.path === 'string' ? p.path : undefined,
    seq: typeof p.seq === 'number' ? p.seq : undefined,
    timestamp: typeof p.timestamp === 'string' ? p.timestamp : undefined,
  }
}

// Split the joined "namespace/project" target on the first slash (both are
// single path segments in Shoka's filesystem layout).
export function splitTarget(target: string): {
  namespace: string
  project: string
} {
  const i = target.indexOf('/')
  if (i < 0) return { namespace: target, project: '' }
  return { namespace: target.slice(0, i), project: target.slice(i + 1) }
}

// The query key that is the "core content" of the currently displayed view.
export function coreKeyForView(view: ViewContext): unknown[] | null {
  if (view.route === 'home') return ['projects']
  if (view.route === 'project' && view.namespace && view.project)
    return ['tree', view.namespace, view.project]
  if (view.route === 'blob' && view.namespace && view.project)
    return ['file', view.namespace, view.project, view.path ?? '']
  return null
}

function sameKey(a: unknown[] | null, b: unknown[] | null): boolean {
  if (!a || !b || a.length !== b.length) return false
  return a.every((v, i) => v === b[i])
}

// Route one NOTIFY event. Performs the cache invalidations directly and returns
// any banner/toast intent for the app to render.
export function routeNotify(
  event: NotifyEvent,
  queryClient: QueryClient,
  view: ViewContext,
): RouterResult {
  const { kind } = event

  if (kind === 'project.create') {
    const key = ['projects']
    if (view.route === 'home') {
      // Displayed core: mark stale without auto-refetch; banner gates the reload.
      queryClient.invalidateQueries({ queryKey: key, refetchType: 'none' })
      return {
        banner: {
          text: 'Projects changed',
          reload: () => void queryClient.refetchQueries({ queryKey: key }),
        },
      }
    }
    queryClient.invalidateQueries({ queryKey: key })
    return {}
  }

  if (kind === 'file.write' || kind === 'file.delete') {
    const { namespace, project } = splitTarget(event.target)
    const path = event.path ?? ''
    const treeKey = ['tree', namespace, project]
    const fileKey = ['file', namespace, project, path]

    const onThisFile =
      view.route === 'blob' &&
      view.namespace === namespace &&
      view.project === project &&
      (view.path ?? '') === path
    const onThisProject =
      view.route === 'project' &&
      view.namespace === namespace &&
      view.project === project

    if (onThisFile) {
      queryClient.invalidateQueries({ queryKey: fileKey, refetchType: 'none' })
      queryClient.invalidateQueries({ queryKey: treeKey }) // sidebar: peripheral
      return {
        banner: {
          text:
            kind === 'file.delete'
              ? 'This file was deleted'
              : 'This file was updated',
          reload: () => void queryClient.refetchQueries({ queryKey: fileKey }),
        },
      }
    }
    if (onThisProject) {
      queryClient.invalidateQueries({ queryKey: treeKey, refetchType: 'none' })
      queryClient.invalidateQueries({ queryKey: fileKey })
      return {
        banner: {
          text: 'Files in this project changed',
          reload: () => void queryClient.refetchQueries({ queryKey: treeKey }),
        },
      }
    }
    // Not the displayed core: silent invalidate (active queries refetch).
    queryClient.invalidateQueries({ queryKey: treeKey })
    queryClient.invalidateQueries({ queryKey: fileKey })
    return {}
  }

  if (kind === 'catalog.invariant_violation') {
    const where = event.path ? `${event.target} (${event.path})` : event.target
    return { toast: { level: 'warn', text: `Catalog inconsistency in ${where}` } }
  }

  // Unknown kind: log and ignore (forward-compatible; §1.6 "no mapping" case).
  if (typeof console !== 'undefined')
    console.warn('notifyRouter: unmapped NOTIFY kind', kind)
  return {}
}

// On reconnect (§1.4 stale-while-revalidate): everything is potentially stale.
// Mark all queries stale without refetching, then background-refetch every
// active query EXCEPT the displayed view's core (never auto-reload displayed
// content — §2); surface a banner so the user pulls the core when ready.
export function routeReconnect(
  queryClient: QueryClient,
  view: ViewContext,
): RouterResult {
  queryClient.invalidateQueries({ refetchType: 'none' })
  const core = coreKeyForView(view)
  void queryClient.refetchQueries({
    type: 'active',
    predicate: (q) => !sameKey(q.queryKey as unknown[], core),
  })
  if (!core) return {}
  return {
    banner: {
      text: 'Reconnected — content may have changed while offline',
      reload: () => void queryClient.refetchQueries({ queryKey: core }),
    },
  }
}
