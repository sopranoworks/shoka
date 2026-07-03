import { describe, it, expect, vi, beforeEach } from 'vitest'

const requestFrame = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ requestFrame }),
}))

import {
  listDomains,
  createDomain,
  updateDomain,
  deleteDomain,
  generateDomainConsent,
} from './domainOps'
import { OAuthDeniedError } from './oauthOps'

beforeEach(() => requestFrame.mockReset())

describe('listDomains', () => {
  it('maps a DOMAIN_LIST frame to its domains array (with the plaintext consent value)', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_LIST',
      payload: {
        domains: [
          {
            id: 'd1',
            domain: 'example.com',
            access_ttl_seconds: 3600,
            refresh_ttl_seconds: 86400,
            consent: 'the-plaintext-value',
          },
        ],
      },
    })
    const out = await listDomains()
    expect(out).toHaveLength(1)
    expect(out[0].domain).toBe('example.com')
    expect(out[0].consent).toBe('the-plaintext-value')
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_LIST', {})
  })

  it('normalises a missing domains field to []', async () => {
    requestFrame.mockResolvedValue({ type: 'DOMAIN_LIST', payload: {} })
    expect(await listDomains()).toEqual([])
  })

  it('throws a typed OAuthDeniedError on an OAUTH_DENIED refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(listDomains()).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('createDomain', () => {
  it('sends the domain + TTLs + scope (defaulting omitted TTLs to 0, scope to *)', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_CREATE',
      payload: { id: 'd1', domain: 'partner.test', access_ttl_seconds: 0, refresh_ttl_seconds: 0, consent: '', scope: '*' },
    })
    await createDomain({ domain: 'partner.test' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_CREATE', {
      domain: 'partner.test',
      access_ttl_seconds: 0,
      refresh_ttl_seconds: 0,
      scope: '*',
    })
  })

  it('forwards explicit TTLs and scope', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_CREATE',
      payload: { id: 'd1', domain: 'partner.test', access_ttl_seconds: 60, refresh_ttl_seconds: 120, consent: '', scope: 'myns:myproj:rw' },
    })
    await createDomain({ domain: 'partner.test', accessTtlSeconds: 60, refreshTtlSeconds: 120, scope: 'myns:myproj:rw' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_CREATE', {
      domain: 'partner.test',
      access_ttl_seconds: 60,
      refresh_ttl_seconds: 120,
      scope: 'myns:myproj:rw',
    })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(createDomain({ domain: 'x.test' })).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('updateDomain — TTL + scope (consent is managed by generate)', () => {
  it('sends TTLs and scope', async () => {
    requestFrame.mockResolvedValue({ type: 'DOMAIN_UPDATE', payload: { id: 'd1' } })
    await updateDomain({ id: 'd1', accessTtlSeconds: 7200, refreshTtlSeconds: 86400, scope: '*' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_UPDATE', {
      id: 'd1',
      access_ttl_seconds: 7200,
      refresh_ttl_seconds: 86400,
      scope: '*',
    })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(
      updateDomain({ id: 'd1', accessTtlSeconds: 0, refreshTtlSeconds: 0, scope: '*' }),
    ).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('generateDomainConsent', () => {
  it('sends the id and returns the freshly minted plaintext consent value', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_GENERATE_CONSENT',
      payload: { id: 'd1', consent: 'fresh-plaintext-value' },
    })
    const out = await generateDomainConsent('d1')
    expect(out.consent).toBe('fresh-plaintext-value')
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_GENERATE_CONSENT', { id: 'd1' })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(generateDomainConsent('d1')).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('deleteDomain', () => {
  it('sends the id and resolves on a DOMAIN_DELETE ack', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_DELETE',
      payload: { id: 'd1', revoked_tokens: 2, status: 'ok' },
    })
    await expect(deleteDomain('d1')).resolves.toBeUndefined()
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_DELETE', { id: 'd1' })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(deleteDomain('d1')).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})
