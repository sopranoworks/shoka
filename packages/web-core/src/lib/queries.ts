import { useQuery } from '@tanstack/react-query'
import { wsClient } from './wsClient'
import { listConnections } from './oauthOps'
import { listDomains } from './domainOps'
import { listConfidentialClients } from './confidentialOps'
import type { ProjectInfo } from './types'

// Reusable read-queries shared by the core screens (OAuth management + the project
// list used by user/namespace management). The document-tree/file/history queries live
// in the app (web/src/lib/queries.ts), which re-exports these names for its own callers.

// The OAuth connections list query key — invalidated after a revoke so the list
// reflects the store. There is no oauth NOTIFY (the store emits none), so a
// live-refresh is manual: invalidate-on-revoke is the floor, plus an explicit
// Refresh action on the management view.
export const OAUTH_CONNECTIONS_KEY = ['oauth-connections'] as const

// The live OAuth/MCP connections, for the admin management view. `enabled` gates
// the fetch on the admin predicate so a non-admin never even issues OAUTH_LIST.
export function useConnectionsQuery(enabled = true) {
  return useQuery({
    queryKey: OAUTH_CONNECTIONS_KEY,
    enabled,
    queryFn: () => listConnections(),
  })
}

// B-71 Stage 2d: the dynamic "domain" entries (trusted domain + per-domain TTL + a
// consent-set indicator) for the domain-mode management screen. Admin-gated like the
// connections query; invalidated after a create/update/delete.
export const OAUTH_DOMAINS_KEY = ['oauth-domains'] as const

export function useDomainsQuery(enabled = true) {
  return useQuery({
    queryKey: OAUTH_DOMAINS_KEY,
    enabled,
    queryFn: () => listDomains(),
  })
}

// B-71 Stage 3: the confidential pre-issued clients (Client ID + Secret) for the
// confidential-mode management screen. Admin-gated like the domains query; invalidated after an
// issue/revoke. The secret is never in this list (only on the issue response, once).
export const OAUTH_CLIENTS_KEY = ['oauth-confidential-clients'] as const

export function useConfidentialClientsQuery(enabled = true) {
  return useQuery({
    queryKey: OAUTH_CLIENTS_KEY,
    enabled,
    queryFn: () => listConfidentialClients(),
  })
}

// The project list (['projects']). Read over /ws/ui and shared by the user/namespace
// management screens (and the app's document UI, via the web/ re-export).
export function useProjectsQuery() {
  return useQuery({
    queryKey: ['projects'],
    queryFn: () => wsClient().request<ProjectInfo[]>('GET_PROJECTS', {}),
  })
}
