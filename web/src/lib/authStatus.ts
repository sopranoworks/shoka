import { useQuery } from '@tanstack/react-query'
import { getStatus, type AuthStatus } from './authClient'

// useAuthStatus exposes the current session's /auth/status (B-28). It is the source
// of the principal used to permission-filter the Settings item list (is_admin = a
// super-user). Cached under a stable key so the gear/list and the user-management
// screen share one fetch.
export function useAuthStatus() {
  return useQuery<AuthStatus>({
    queryKey: ['auth-status'],
    queryFn: getStatus,
    staleTime: 30_000,
  })
}

// useIsSuperUser reports whether the current session is a super-user (admin over all
// namespaces) — the gate for the user-management settings item.
export function useIsSuperUser(): boolean {
  const { data } = useAuthStatus()
  return !!data?.principal?.is_admin
}
