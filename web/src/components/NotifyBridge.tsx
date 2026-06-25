import { useEffect, useRef } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { wsClient } from '@shoka/web-core'
import {
  routeNotify,
  routeReconnect,
  parseNotifyEvent,
  type ViewContext,
} from '@shoka/web-core'
import { deriveViewContext } from '../lib/viewContext'
import { useBanner, useToast, useEditSignal } from '@shoka/web-core'

// Non-visual: wires the /ws/ui NOTIFY stream and reconnect events into the
// query cache + banner/toast UI, and clears the banner on navigation. Mounted
// once in the persistent Shell (inside the router, so it can read the route).
export function NotifyBridge() {
  const queryClient = useQueryClient()
  // Destructure the STABLE callbacks (useCallback) so the effects below run
  // once — depending on the context object would re-run them on every banner
  // change, resetting the reconnect flag mid-reconnect.
  const { show: showBanner, clear: clearBanner } = useBanner()
  const { add: addToast } = useToast()
  const { set: setEditSignal, clear: clearEditSignal } = useEditSignal()
  const navigate = useNavigate()
  const pathname = useRouterState({ select: (s) => s.location.pathname })

  // Latest view context, read by the NOTIFY handler without re-subscribing.
  const viewRef = useRef<ViewContext>(deriveViewContext(pathname))
  viewRef.current = deriveViewContext(pathname)

  // Route inbound NOTIFY frames.
  useEffect(() => {
    wsClient().setNotifyHandler((payload) => {
      const event = parseNotifyEvent(payload)
      if (!event) return
      const res = routeNotify(event, queryClient, viewRef.current)
      if (res.banner) showBanner(res.banner)
      if (res.toast) addToast(res.toast)
      if (res.editSignal) setEditSignal(res.editSignal)
      if (res.follow) {
        // The open view of a moved file follows to the new path, preserving
        // mode. For the edit route this is buffer-safe — EditorPage keeps its
        // initialized buffer across the same-route param change, so unsaved
        // edits ride along. clearEditSignal() drops any prior edit-route signal
        // so it does not linger against the new path.
        clearEditSignal()
        void navigate({
          to:
            res.follow.route === 'edit'
              ? '/p/$namespace/$project/edit/$'
              : '/p/$namespace/$project/blob/$',
          params: {
            namespace: res.follow.namespace,
            project: res.follow.project,
            _splat: res.follow.path,
          },
        })
      }
    })
    return () => wsClient().setNotifyHandler(() => {})
  }, [queryClient, showBanner, addToast, setEditSignal, clearEditSignal, navigate])

  // Route PERMISSION_DENIED (the B-28 stage-2 authz refusal) into a non-fatal toast:
  // a user whose scope lacks the required level (e.g. a read-only user attempting a
  // write) sees a clear reason; the app keeps running.
  useEffect(() => {
    wsClient().setDenyHandler((payload) => {
      const p = (payload ?? {}) as { message?: string; op?: string }
      addToast({ level: 'warn', text: p.message || `You do not have permission for ${p.op ?? 'this action'}.` })
    })
    return () => wsClient().setDenyHandler(() => {})
  }, [addToast])

  // Reconnect → stale-while-revalidate. Only after a real disconnect (not the
  // initial connect): track whether a disconnect happened since last connected.
  useEffect(() => {
    let hadDisconnect = false
    const init = wsClient().getState().status
    if (init === 'reconnecting' || init === 'disconnected') hadDisconnect = true
    return wsClient().subscribeState((s) => {
      if (s.status === 'reconnecting' || s.status === 'disconnected') {
        hadDisconnect = true
      } else if (s.status === 'connected') {
        if (hadDisconnect) {
          const res = routeReconnect(queryClient, viewRef.current)
          if (res.banner) showBanner(res.banner)
        }
        hadDisconnect = false
      }
    })
  }, [queryClient, showBanner])

  // Navigation clears the banner and any edit-route external-change signal
  // (both are tied to the view that raised them).
  useEffect(() => {
    clearBanner()
    clearEditSignal()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pathname])

  return null
}
