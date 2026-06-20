import { describe, it, expect, vi, beforeEach } from 'vitest'

const requestFrame = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ requestFrame }),
}))

import {
  listConfidentialClients,
  issueConfidentialClient,
  revokeConfidentialClient,
} from './confidentialOps'
import { OAuthDeniedError } from './oauthOps'

beforeEach(() => requestFrame.mockReset())

describe('listConfidentialClients', () => {
  it('maps a CLIENT_LIST frame to its clients array (never a secret)', async () => {
    requestFrame.mockResolvedValue({
      type: 'CLIENT_LIST',
      payload: {
        clients: [
          {
            id: 'e1',
            client_id: 'cid-1',
            scope: 'namespace:foo:rw',
            expires_at: '2026-09-01T00:00:00Z',
            created_at: '2026-06-20T00:00:00Z',
          },
        ],
      },
    })
    const out = await listConfidentialClients()
    expect(out).toHaveLength(1)
    expect(out[0].client_id).toBe('cid-1')
    expect(JSON.stringify(out)).not.toContain('client_secret')
    expect(requestFrame).toHaveBeenCalledWith('CLIENT_LIST', {})
  })

  it('normalises a missing clients field to []', async () => {
    requestFrame.mockResolvedValue({ type: 'CLIENT_LIST', payload: {} })
    expect(await listConfidentialClients()).toEqual([])
  })

  it('throws OAuthDeniedError on an OAUTH_DENIED refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(listConfidentialClients()).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('issueConfidentialClient', () => {
  it('sends the scope + validity seconds and returns the once-shown secret', async () => {
    requestFrame.mockResolvedValue({
      type: 'CLIENT_ISSUE',
      payload: {
        id: 'e1',
        client_id: 'cid-1',
        scope: 'namespace:foo:rw',
        expires_at: '2026-09-01T00:00:00Z',
        created_at: '2026-06-20T00:00:00Z',
        client_secret: 'the-raw-secret-shown-once',
      },
    })
    const out = await issueConfidentialClient({ scope: 'namespace:foo:rw', validitySeconds: 2592000 })
    expect(out.client_secret).toBe('the-raw-secret-shown-once')
    expect(requestFrame).toHaveBeenCalledWith('CLIENT_ISSUE', {
      scope: 'namespace:foo:rw',
      validity_seconds: 2592000,
    })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(
      issueConfidentialClient({ scope: '*', validitySeconds: 60 }),
    ).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})

describe('revokeConfidentialClient', () => {
  it('sends the id and resolves on a CLIENT_REVOKE ack', async () => {
    requestFrame.mockResolvedValue({
      type: 'CLIENT_REVOKE',
      payload: { id: 'e1', revoked_tokens: 1, status: 'ok' },
    })
    await expect(revokeConfidentialClient('e1')).resolves.toBeUndefined()
    expect(requestFrame).toHaveBeenCalledWith('CLIENT_REVOKE', { id: 'e1' })
  })

  it('throws OAuthDeniedError on a refusal', async () => {
    requestFrame.mockResolvedValue({
      type: 'OAUTH_DENIED',
      payload: { reason: 'forbidden', message: 'admin only' },
    })
    await expect(revokeConfidentialClient('e1')).rejects.toBeInstanceOf(OAuthDeniedError)
  })
})
