import { describe, it, expect, vi, beforeEach } from 'vitest'

const request = vi.fn()
vi.mock('./wsClient', () => ({
  wsClient: () => ({ request }),
}))

import { setUserPassword } from './adminOps'

beforeEach(() => request.mockReset())

describe('setUserPassword', () => {
  it('sends ADMIN_SET_USER_PASSWORD with the target email + new password', async () => {
    request.mockResolvedValue({ status: 'ok' })
    await setUserPassword('bob@example.com', 'newpassword1')
    expect(request).toHaveBeenCalledWith('ADMIN_SET_USER_PASSWORD', {
      email: 'bob@example.com',
      password: 'newpassword1',
    })
  })

  it('rejects with the server message when the caller is not an admin', async () => {
    request.mockImplementationOnce(async () => {
      throw new Error('permission denied')
    })
    await expect(setUserPassword('bob@example.com', 'newpassword1')).rejects.toThrow(
      'permission denied',
    )
  })
})
