import { useEffect, useState } from 'react'
import { wsClient, type ConnState } from '@shoka/web-core'

// Subscribe to the /ws/ui client's connection state for the status indicator.
export function useConnectionStatus(): ConnState {
  const [state, setState] = useState<ConnState>(() => wsClient().getState())
  useEffect(() => {
    setState(wsClient().getState())
    return wsClient().subscribeState(setState)
  }, [])
  return state
}
