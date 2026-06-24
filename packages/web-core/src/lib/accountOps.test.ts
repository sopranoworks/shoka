import { describe, it, expect, vi, beforeEach } from 'vitest'

const request = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ request }),
}))

import { getAccount, setAccountName, setAccountPassword } from './accountOps'

beforeEach(() => request.mockReset())

describe('getAccount', () => {
  it('requests ACCOUNT_GET with no target and returns the own-account info', async () => {
    request.mockResolvedValue({
      email: 'me@example.com',
      display_name: 'Me',
      scope: 'namespace:foo:rw',
      is_admin: false,
      has_totp: false,
      created_at: '2026-06-20T00:00:00Z',
    })
    const out = await getAccount()
    expect(out.email).toBe('me@example.com')
    expect(request).toHaveBeenCalledWith('ACCOUNT_GET', {})
  })
})

describe('setAccountName', () => {
  it('sends only the display name (no target email) — self-access is structural', async () => {
    request.mockResolvedValue({ email: 'me@example.com', display_name: 'New' })
    await setAccountName('New')
    expect(request).toHaveBeenCalledWith('ACCOUNT_SET_NAME', { display_name: 'New' })
    // The payload must NOT carry an email/target field.
    const payload = request.mock.calls[0][1] as Record<string, unknown>
    expect(payload).not.toHaveProperty('email')
  })

  it('rejects with the server message on an ERROR (e.g. empty name)', async () => {
    request.mockImplementationOnce(async () => {
      throw new Error('display name must not be empty')
    })
    await expect(setAccountName('  ')).rejects.toThrow('display name must not be empty')
  })
})

describe('setAccountPassword', () => {
  it('sends current + new password (no target email)', async () => {
    request.mockResolvedValue({ status: 'ok' })
    await setAccountPassword('old', 'newpassword1')
    expect(request).toHaveBeenCalledWith('ACCOUNT_SET_PASSWORD', {
      current_password: 'old',
      new_password: 'newpassword1',
    })
    const payload = request.mock.calls[0][1] as Record<string, unknown>
    expect(payload).not.toHaveProperty('email')
  })

  it('rejects with the server message when the current password is wrong', async () => {
    request.mockImplementationOnce(async () => {
      throw new Error('current password is incorrect')
    })
    await expect(setAccountPassword('bad', 'newpassword1')).rejects.toThrow(
      'current password is incorrect',
    )
  })
})
