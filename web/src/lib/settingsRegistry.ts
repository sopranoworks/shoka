// The Settings view's item registry (B-28 stage 3, extended in part 2). Settings is an
// EXTENSIBLE framework: future items (own profile, password change, TOTP/passkey
// management) are added here. Items are permission-filtered by a visibility predicate over
// the viewer's auth state. The Settings view (the gear) itself is always present for every
// user; only this item list is filtered.

// SettingsVisibility is the viewer's auth state the visibility predicates read.
export interface SettingsVisibility {
  // isSuperUser: a wildcard-admin principal (admin over all namespaces), or the no-lockout
  // empty-store single operator.
  isSuperUser: boolean
  // managesAnyNamespace: super-user OR a namespace-admin of ≥1 namespace (server-derived,
  // /auth/status manages_any_namespace) — the predicate the ns/proj-management item uses.
  managesAnyNamespace: boolean
}

export interface SettingsItem {
  id: string
  label: string
  // visible decides whether this item appears for the given viewer.
  visible: (v: SettingsVisibility) => boolean
}

export const SETTINGS_ITEMS: SettingsItem[] = [
  // My Account is the per-user self-service page — visible to EVERY authenticated user
  // (NOT super-user-only). The server enforces self-access structurally (the ACCOUNT_*
  // ops act on the session identity only), so it is safe for all viewers.
  { id: 'account', label: 'My Account', visible: () => true },
  // User management + OAuth connections are SUPER-USER-ONLY.
  { id: 'users', label: 'User management', visible: (v) => v.isSuperUser },
  // The OAuth/MCP connection management screen — its real home now. The OAUTH_* ops are
  // admin-gated server-side (stages 2/4), so this filter is the UI half.
  { id: 'oauth', label: 'OAuth connections', visible: (v) => v.isSuperUser },
  // Namespace / project management (B-28 part 2) — visible to a super-user OR ANY
  // namespace-admin (NOT super-user-only), the server-derived manages-any-namespace
  // predicate. The screen's per-op controls are further gated (namespace add/delete =
  // super-user; project add/delete = admin-on-ns), and the server is authoritative.
  {
    id: 'namespaces',
    label: 'Namespace / project management',
    visible: (v) => v.isSuperUser || v.managesAnyNamespace,
  },
  // ask_the_librarian health (B-73) — SUPER-USER-ONLY: it reports server-wide LLM
  // config validity (provider/model/connectivity), never a secret. The LIBRARIAN_*
  // ws ops are admin-gated server-side, so this filter is the UI half.
  { id: 'librarian', label: 'Librarian', visible: (v) => v.isSuperUser },
]

// visibleSettingsItems returns the items the current viewer may access.
export function visibleSettingsItems(v: SettingsVisibility): SettingsItem[] {
  return SETTINGS_ITEMS.filter((it) => it.visible(v))
}
