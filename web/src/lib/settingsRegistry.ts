// The Settings view's item registry (B-28 stage 3). Settings is an EXTENSIBLE
// framework: future items (own profile, password change, TOTP/passkey management)
// are added here. Items are permission-filtered — `superUserOnly` items appear only
// for a super-user (a wildcard-admin principal). Stage 3 ships the framework + the
// one super-user-only item, "User management". The Settings view (the gear) itself is
// always present for every user; only this item list is filtered.

export interface SettingsItem {
  id: string
  label: string
  // superUserOnly items appear only for a super-user (admin over all namespaces).
  // Future self-service items will set this false so every user sees them.
  superUserOnly: boolean
}

export const SETTINGS_ITEMS: SettingsItem[] = [
  { id: 'users', label: 'User management', superUserOnly: true },
]

// visibleSettingsItems returns the items the current principal may access.
export function visibleSettingsItems(isSuperUser: boolean): SettingsItem[] {
  return SETTINGS_ITEMS.filter((it) => !it.superUserOnly || isSuperUser)
}
