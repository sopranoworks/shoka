import { wsClient } from './wsClient'
import { OAuthDeniedError } from './oauthOps'
import type { DomainInfo, DomainListPayload, OAuthDenied } from './types'

// B-71 Stage 2d — the admin-only DOMAIN_* /ws/ui ops for domain-mode management: CRUD over
// the dynamic "domain" registration store (trusted domain + per-domain TTL + per-domain
// consent). The per-domain CONSENT is write-only: a raw value is accepted on create/update
// and hashed at rest by the server; it is NEVER returned (only a consent_set indicator).
// Refusals come back as an OAUTH_DENIED frame, surfaced as OAuthDeniedError (mirrors oauthOps).

function throwIfDenied(type: string, payload: unknown): void {
  if (type === 'OAUTH_DENIED') {
    const p = payload as OAuthDenied
    throw new OAuthDeniedError(p.reason, p.message)
  }
}

export async function listDomains(): Promise<DomainInfo[]> {
  const frame = await wsClient().requestFrame('DOMAIN_LIST', {})
  throwIfDenied(frame.type, frame.payload)
  return (frame.payload as DomainListPayload).domains ?? []
}

export interface DomainCreateInput {
  domain: string
  accessTtlSeconds?: number
  refreshTtlSeconds?: number
  consent?: string // optional; hashed on write; never returned
}

export async function createDomain(input: DomainCreateInput): Promise<DomainInfo> {
  const frame = await wsClient().requestFrame('DOMAIN_CREATE', {
    domain: input.domain,
    access_ttl_seconds: input.accessTtlSeconds ?? 0,
    refresh_ttl_seconds: input.refreshTtlSeconds ?? 0,
    consent: input.consent ?? '',
  })
  throwIfDenied(frame.type, frame.payload)
  return frame.payload as DomainInfo
}

export interface DomainUpdateInput {
  id: string
  accessTtlSeconds: number
  refreshTtlSeconds: number
  // setConsent: undefined = leave unchanged; '' = CLEAR the per-domain consent; a value = SET
  // it (hashed). The raw value is never read back.
  setConsent?: string
}

export async function updateDomain(input: DomainUpdateInput): Promise<DomainInfo> {
  const payload: Record<string, unknown> = {
    id: input.id,
    access_ttl_seconds: input.accessTtlSeconds,
    refresh_ttl_seconds: input.refreshTtlSeconds,
  }
  if (input.setConsent !== undefined) payload.set_consent = input.setConsent
  const frame = await wsClient().requestFrame('DOMAIN_UPDATE', payload)
  throwIfDenied(frame.type, frame.payload)
  return frame.payload as DomainInfo
}

export async function deleteDomain(id: string): Promise<void> {
  const frame = await wsClient().requestFrame('DOMAIN_DELETE', { id })
  throwIfDenied(frame.type, frame.payload)
}
