import { describe, it, expect, vi, beforeEach } from 'vitest'

const requestFrame = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ requestFrame }),
}))

import {
  listConnections,
  revokeConnection,
  issueSelfToken,
  clientDomain,
  OAuthDeniedError,
} from './oauthOps'

describe('issueSelfToken', () => {
  beforeEach(() => requestFrame.mockReset())

  it('sends the chosen per-issuance finite expiry (B-71 Stage 4)', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_ISSUE_SELF',
      payload: { access_token: 'tok', access_expiry: '2026-07-01T00:00:00Z' },
    })
    await issueSelfToken(604800)
    expect(requestFrame).toHaveBeenCalledWith('OAUTH_ISSUE_SELF', { validity_seconds: 604800 })
  })

  it('defaults to 0 (the finite global default sentinel) when omitted', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_ISSUE_SELF',
      payload: { access_token: 'tok', access_expiry: '2026-07-01T00:00:00Z' },
    })
    await issueSelfToken()
    expect(requestFrame).toHaveBeenCalledWith('OAUTH_ISSUE_SELF', { validity_seconds: 0 })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(issueSelfToken()).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('listConnections', () => {
  beforeEach(() => requestFrame.mockReset())

  it('maps an OAUTH_LIST frame to its connections array', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_LIST',
      payload: {
        connections: [
          {
            series_id: 's1',
            series_id_short: 's1short',
            client_id: 'https://client.example.com/cimd',
            principal_name: 'Op',
            principal_email: 'op@example.test',
            issued_at: '2026-06-03T12:00:00Z',
            access_expiry: '2026-06-03T13:00:00Z',
          },
        ],
      },
    })
    const out = await listConnections()
    expect(out).toHaveLength(1)
    expect(out[0].series_id).toBe('s1')
    expect(requestFrame).toHaveBeenCalledWith('OAUTH_LIST', {})
  })

  it('normalises a missing connections field to []', async () => {
    requestFrame.mockResolvedValue({ type: 'OAUTH_LIST', payload: {} })
    expect(await listConnections()).toEqual([])
  })

  it('throws a typed OAuthDeniedError on an OAUTH_DENIED refusal (forbidden)', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(listConnections()).rejects.toBeInstanceOf(OAuthDeniedError)
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'oauth_disabled', message: 'off' },
    })
    await expect(listConnections()).rejects.toMatchObject({
      reason: 'oauth_disabled',
    })
  })
})

describe('revokeConnection', () => {
  beforeEach(() => requestFrame.mockReset())

  it('sends the series_id and resolves on an OAUTH_REVOKE ack', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_REVOKE',
      payload: { series_id: 's1', status: 'ok' },
    })
    await expect(revokeConnection('s1')).resolves.toBeUndefined()
    expect(requestFrame).toHaveBeenCalledWith('OAUTH_REVOKE', {
      series_id: 's1',
    })
  })

  it('throws OAuthDeniedError on an OAUTH_DENIED refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(revokeConnection('s1')).rejects.toBeInstanceOf(
      OAuthDeniedError,
    )
  })
})

describe('clientDomain', () => {
  it('extracts the host from a CIMD metadata URL', () => {
    expect(clientDomain('https://client.example.com/.well-known/cimd')).toBe(
      'client.example.com',
    )
  })
  it('falls back to the raw value when not a URL', () => {
    expect(clientDomain('not-a-url')).toBe('not-a-url')
  })
})
