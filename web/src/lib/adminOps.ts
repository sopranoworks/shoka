import { wsClient } from './wsClient'

// Admin user-management ops over /ws/ui (B-28 stage 3). All are super-user-only,
// enforced server-side by the stage-2 dispatch gate; a non-super-user request is
// rejected (the request promise rejects with "permission denied"). The destructive
// ones are additionally self-guarded server-side.

export interface UserInfo {
  email: string
  display_name: string
  scope: string
}

export interface InviteInfo {
  code_hash: string
  email: string
  scope: string
  expiry: string
  used: boolean
  created_at: string
}

export interface InviteCreated {
  code: string
  email: string
  scope: string
  expiry: string
  code_hash: string
}

export function listUsers(): Promise<{ users: UserInfo[] }> {
  return wsClient().request('ADMIN_LIST_USERS', {})
}

export function setUserScope(email: string, scope: string): Promise<{ status: string }> {
  return wsClient().request('ADMIN_SET_USER_SCOPE', { email, scope })
}

export function removeUser(email: string): Promise<{ status: string }> {
  return wsClient().request('ADMIN_REMOVE_USER', { email })
}

export function createInvite(email: string, scope: string, ttlHours?: number): Promise<InviteCreated> {
  return wsClient().request('ADMIN_CREATE_INVITE', { email, scope, ttl_hours: ttlHours })
}

export function listInvites(): Promise<{ invites: InviteInfo[] }> {
  return wsClient().request('ADMIN_LIST_INVITES', {})
}

export function revokeInvite(codeHash: string): Promise<{ status: string }> {
  return wsClient().request('ADMIN_REVOKE_INVITE', { code_hash: codeHash })
}
