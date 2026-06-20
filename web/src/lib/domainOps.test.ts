import { describe, it, expect, vi, beforeEach } from 'vitest'

const requestFrame = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ requestFrame }),
}))

import { listDomains, createDomain, updateDomain, deleteDomain } from './domainOps'
import { OAuthDeniedError } from './oauthOps'

beforeEach(() => requestFrame.mockReset())

describe('listDomains', () => {
  it('maps a DOMAIN_LIST frame to its domains array', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_LIST',
      payload: {
        domains: [
          {
            id: 'd1',
            domain: 'example.com',
            access_ttl_seconds: 3600,
            refresh_ttl_seconds: 86400,
            consent_set: true,
          },
        ],
      },
    })
    const out = await listDomains()
    expect(out).toHaveLength(1)
    expect(out[0].domain).toBe('example.com')
    expect(out[0].consent_set).toBe(true)
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
  it('sends the domain + TTLs, defaulting omitted TTL/consent to 0/""', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_CREATE',
      payload: { id: 'd1', domain: 'partner.test', access_ttl_seconds: 0, refresh_ttl_seconds: 0, consent_set: false },
    })
    await createDomain({ domain: 'partner.test' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_CREATE', {
      domain: 'partner.test',
      access_ttl_seconds: 0,
      refresh_ttl_seconds: 0,
      consent: '',
    })
  })

  it('forwards an explicit consent value (hashed server-side, never read back)', async () => {
    requestFrame.mockResolvedValue({
      type: 'DOMAIN_CREATE',
      payload: { id: 'd1', domain: 'partner.test', access_ttl_seconds: 60, refresh_ttl_seconds: 120, consent_set: true },
    })
    await createDomain({ domain: 'partner.test', accessTtlSeconds: 60, refreshTtlSeconds: 120, consent: 's3cret' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_CREATE', {
      domain: 'partner.test',
      access_ttl_seconds: 60,
      refresh_ttl_seconds: 120,
      consent: 's3cret',
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

describe('updateDomain — write-only consent semantics', () => {
  it('OMITS set_consent when setConsent is undefined (leave unchanged)', async () => {
    requestFrame.mockResolvedValue({ type: 'DOMAIN_UPDATE', payload: { id: 'd1' } })
    await updateDomain({ id: 'd1', accessTtlSeconds: 7200, refreshTtlSeconds: 86400 })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_UPDATE', {
      id: 'd1',
      access_ttl_seconds: 7200,
      refresh_ttl_seconds: 86400,
    })
  })

  it('sends set_consent: "" to CLEAR the per-domain consent', async () => {
    requestFrame.mockResolvedValue({ type: 'DOMAIN_UPDATE', payload: { id: 'd1' } })
    await updateDomain({ id: 'd1', accessTtlSeconds: 0, refreshTtlSeconds: 0, setConsent: '' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_UPDATE', {
      id: 'd1',
      access_ttl_seconds: 0,
      refresh_ttl_seconds: 0,
      set_consent: '',
    })
  })

  it('sends set_consent: <value> to SET a new per-domain consent', async () => {
    requestFrame.mockResolvedValue({ type: 'DOMAIN_UPDATE', payload: { id: 'd1' } })
    await updateDomain({ id: 'd1', accessTtlSeconds: 0, refreshTtlSeconds: 0, setConsent: 'new-secret' })
    expect(requestFrame).toHaveBeenCalledWith('DOMAIN_UPDATE', {
      id: 'd1',
      access_ttl_seconds: 0,
      refresh_ttl_seconds: 0,
      set_consent: 'new-secret',
    })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(
      updateDomain({ id: 'd1', accessTtlSeconds: 0, refreshTtlSeconds: 0 }),
    ).rejects.toBeInstanceOf(OAuthDeniedError)
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
