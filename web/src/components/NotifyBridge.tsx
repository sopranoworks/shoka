import { useEffect, useRef } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { useRouterState } from '@tanstack/react-router'
import { wsClient } from '../lib/wsClient'
import {
  routeNotify,
  routeReconnect,
  parseNotifyEvent,
  type ViewContext,
} from '../lib/notifyRouter'
import { deriveViewContext } from '../lib/viewContext'
import { useBanner } from '../lib/banner'
import { useToast } from '../lib/toast'
import { useEditSignal } from '../lib/editSignal'

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
    })
    return () => wsClient().setNotifyHandler(() => {})
  }, [queryClient, showBanner, addToast, setEditSignal])

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
