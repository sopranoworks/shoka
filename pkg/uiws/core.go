package uiws

// CoreHandlers is the reusable auth/user/OAuth slice of the /ws/ui surface (the
// 2026-06-21 GitYard core-handler extraction, step (b) of the core-extraction line;
// moved to pkg/uiws by the 2026-06-21 ui-split step).
//
// It holds ONLY the user + OAuth stores — NO document storage.StorageService, no
// drafts/ingest/identity/notify. Its methods are the ACCOUNT_* (My Account), ADMIN_*
// (user management), and OAUTH_*/DOMAIN_*/CLIENT_* (OAuth/domain/confidential)
// handlers. Shoka's *Manager EMBEDS a *CoreHandlers, so every existing call (the
// dispatch switch, SetUserStore/SetOAuthStore, the tests, cmd/shoka) reaches these
// methods unchanged via Go method/field promotion — Shoka's runtime behaviour is
// identical. A SECOND program (GitYard, a feature-reduced Shoka with no document
// store) can construct a CoreHandlers with just the two stores and mount these
// handlers on its OWN ws manager, without supplying a StorageService.
//
// The handler BODIES are document-store-free (they call only the user + OAuth stores);
// living in pkg/uiws makes that independence structural rather than incidental. The
// shared /ws/ui enforcement gate is Client.Gate (protocol.go), table-parameterized so
// the core slice and the document handlers gate through the same decision.
type CoreHandlers struct {
	// oauth is the OAuth connection store for the admin management requests. It is
	// nil when OAuth is disabled (set via SetOAuthStore inside the oauth-enabled
	// wiring), in which case OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied with
	// reason "oauth_disabled" rather than nil-panicking.
	oauth OAuthConnectionStore
	// selfIssuer mints the operator's "token to self" (B-46b §2.2). nil when OAuth
	// is disabled (wired via SetOAuthSelfIssuer in the oauth-enabled startup path),
	// in which case OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
	selfIssuer OAuthSelfIssuer
	// users backs the super-user-only user-management ops (B-28 stage 3) and the
	// self-service My Account ops. nil when no user store is wired (the ADMIN_*/
	// ACCOUNT_* handlers then report it unavailable).
	users UserAdminStore
	// onOAuthChange is called after a successful OAuth mutation (revoke, issue,
	// domain create/update/delete, client issue/revoke). The Manager wires this
	// to publish a notify event so other connected browsers auto-refresh.
	onOAuthChange func()
}

// SetOAuthStore wires the OAuth connection store for the admin management
// requests. Called only in the oauth-enabled startup path; when unset the store
// is nil and OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied ("oauth_disabled").
func (h *CoreHandlers) SetOAuthStore(s OAuthConnectionStore) {
	h.oauth = s
}

// SetOAuthSelfIssuer wires the token-to-self minter for OAUTH_ISSUE_SELF. Called
// only in the oauth-enabled startup path; when unset the issuer is nil and
// OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
func (h *CoreHandlers) SetOAuthSelfIssuer(i OAuthSelfIssuer) {
	h.selfIssuer = i
}

// SetUserStore wires the user store for the super-user-only user-management ops
// (B-28 stage 3). Called in startup; when unset the ADMIN_* handlers report the
// capability unavailable.
func (h *CoreHandlers) SetUserStore(u UserAdminStore) {
	h.users = u
}

// SetOnOAuthChange registers a callback invoked after every successful OAuth
// mutation. The Manager uses this to publish a notify event so other connected
// browsers auto-refresh their OAuth views.
func (h *CoreHandlers) SetOnOAuthChange(f func()) {
	h.onOAuthChange = f
}

func (h *CoreHandlers) notifyOAuthChange() {
	if h.onOAuthChange != nil {
		h.onOAuthChange()
	}
}
