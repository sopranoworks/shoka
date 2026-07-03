import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { getStatus, type AuthStatus } from '../lib/authClient'
import { AUTH_STATUS_KEY, seedAuthStatus, useAuthStatus } from '../lib/authStatus'
import { wsClient } from '../lib/wsClient'
import { FirstRunWizard } from './FirstRunWizard'
import { LoginScreen } from './LoginScreen'

export function AuthGate({ appName, children }: { appName: string; children: ReactNode }) {
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<AuthStatus | null>(null)
  const [failed, setFailed] = useState(false)

  function refresh() {
    setFailed(false)
    getStatus()
      .then((s) => {
        seedAuthStatus(queryClient, s)
        setStatus(s)
      })
      .catch(() => setFailed(true))
  }
  useEffect(refresh, [])

  // Track live auth status from the polling query. When the session expires,
  // useAuthStatus detects it (via staleTime-based refetch or WS-triggered
  // invalidation) and we update the gate state to redirect to login.
  const { data: liveStatus } = useAuthStatus()
  useEffect(() => {
    if (liveStatus) setStatus(liveStatus)
  }, [liveStatus])

  const showApp = status != null && shouldShowApp(status)

  // Connect WS when authenticated; close it when the session expires.
  const wasShowing = useRef(false)
  useEffect(() => {
    if (showApp) {
      wsClient().connect()
      wasShowing.current = true
    } else if (wasShowing.current) {
      wsClient().close()
      wasShowing.current = false
    }
  }, [showApp])

  // When the WS connection drops unexpectedly, invalidate the auth-status query
  // to trigger an immediate recheck rather than waiting for the next stale window.
  useEffect(() => {
    if (!showApp) return
    return wsClient().subscribeState((s) => {
      if (s.status === 'reconnecting' || s.status === 'disconnected') {
        queryClient.invalidateQueries({ queryKey: AUTH_STATUS_KEY })
      }
    })
  }, [showApp, queryClient])

  if (failed) {
    return (
      <div role="alert" style={{ padding: '2rem', textAlign: 'center' }}>
        Could not reach the server. <button onClick={refresh}>Retry</button>
      </div>
    )
  }
  if (status == null) return null

  if (!status.users_exist) {
    if (status.first_run_allowed) {
      return <FirstRunWizard appName={appName} passkeyEnabled={status.passkey_enabled} onDone={refresh} />
    }
    return <>{children}</>
  }
  if (!status.authenticated) {
    return <LoginScreen appName={appName} passkeyEnabled={status.passkey_enabled} onDone={refresh} />
  }
  return <>{children}</>
}

function shouldShowApp(s: AuthStatus): boolean {
  if (!s.users_exist) return !s.first_run_allowed
  return s.authenticated
}
