package uiws

import "encoding/json"

// Dispatch routes one already-authorized /ws/ui message to the matching core handler
// (the ACCOUNT_*/ADMIN_*/OAUTH_*/DOMAIN_*/CLIENT_* slice) and reports whether it owned
// the message type. The caller runs the gate (Client.Gate) ONCE before calling Dispatch
// and, when Dispatch returns false, falls through to its own (document / Git-PR) switch
// — so a consumer composes the core ops with its own without duplicating the core cases.
// This is exactly how Shoka's *Manager and a second program (GitYard) both mount the
// core: gate, then Dispatch, then their own handlers. The handler bodies are unchanged
// from the pre-extraction internal/ui dispatch.
func (h *CoreHandlers) Dispatch(client *Client, msgType MessageType, payload json.RawMessage) bool {
	switch msgType {
	case MsgOAuthList:
		h.handleOAuthList(client)
	case MsgOAuthRevoke:
		h.handleOAuthRevoke(client, payload)
	case MsgOAuthIssueSelf:
		h.handleOAuthIssueSelf(client, payload)
	case MsgDomainList:
		h.handleDomainList(client)
	case MsgDomainCreate:
		h.handleDomainCreate(client, payload)
	case MsgDomainUpdate:
		h.handleDomainUpdate(client, payload)
	case MsgDomainDelete:
		h.handleDomainDelete(client, payload)
	case MsgDomainGenerateConsent:
		h.handleDomainGenerateConsent(client, payload)
	case MsgClientList:
		h.handleConfidentialList(client)
	case MsgClientIssue:
		h.handleConfidentialIssue(client, payload)
	case MsgClientRevoke:
		h.handleConfidentialRevoke(client, payload)
	case MsgAccountGet:
		h.handleAccountGet(client)
	case MsgAccountSetName:
		h.handleAccountSetName(client, payload)
	case MsgAccountSetPassword:
		h.handleAccountSetPassword(client, payload)
	case MsgAdminListUsers:
		h.handleAdminListUsers(client)
	case MsgAdminSetUserScope:
		h.handleAdminSetUserScope(client, payload)
	case MsgAdminSetUserEnabled:
		h.handleAdminSetUserEnabled(client, payload)
	case MsgAdminSetUserPassword:
		h.handleAdminSetUserPassword(client, payload)
	case MsgAdminRemoveUser:
		h.handleAdminRemoveUser(client, payload)
	case MsgAdminCreateInvite:
		h.handleAdminCreateInvite(client, payload)
	case MsgAdminListInvites:
		h.handleAdminListInvites(client)
	case MsgAdminRevokeInvite:
		h.handleAdminRevokeInvite(client, payload)
	default:
		return false
	}
	return true
}
