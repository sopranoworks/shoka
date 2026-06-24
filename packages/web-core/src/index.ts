// @shoka/web-core — the reusable core-screen surface.
//
// A second product (e.g. GitYard) imports the auth/user/OAuth/settings/namespace
// screens, the settings framework, the WebSocket client + ops, and the shared hooks
// from here, and extends them via CoreScreensProvider — without modifying this package.
// The design tokens are a separate import: `@shoka/web-core/tokens.css`.

// --- screens ---------------------------------------------------------------
export { MyAccountPage } from './pages/MyAccountPage'
export { UserManagementPage } from './pages/UserManagementPage'
export { ConnectionsPage } from './pages/ConnectionsPage'
export { NamespaceManagementPage } from './pages/NamespaceManagementPage'
export { LibrarianStatusPage } from './pages/LibrarianStatusPage'
export { SettingsPage } from './pages/SettingsPage'

// --- settings framework ----------------------------------------------------
export { SettingsItemList } from './components/SettingsItemList'

// --- reusable dialogs (used by the management screen; also consumed by the app) ---
export { PromptDialog } from './components/PromptDialog'
export { RenameDialog } from './components/RenameDialog'
export { MoveProjectDialog } from './components/MoveProjectDialog'
export { TypeToConfirmDialog } from './components/TypeToConfirmDialog'
export {
  SETTINGS_ITEMS,
  visibleSettingsItems,
  type SettingsItem,
  type SettingsVisibility,
} from './lib/settingsRegistry'

// --- extension seam --------------------------------------------------------
export {
  CoreScreensProvider,
  useCoreScreens,
  type CoreScreensConfig,
} from './lib/coreScreens'

// --- WebSocket client + protocol -------------------------------------------
export * from './lib/wsClient'

// --- shared payload types --------------------------------------------------
export * from './lib/types'

// --- auth hooks ------------------------------------------------------------
export * from './lib/authClient'
export * from './lib/authStatus'
export * from './lib/admin'

// --- toast / notify --------------------------------------------------------
export * from './lib/toast'
export * from './lib/notifyRouter'

// --- ops + reusable queries ------------------------------------------------
export * from './lib/accountOps'
// adminOps minus InviteInfo: the public InviteInfo is authClient's (redeem-side); adminOps'
// admin-list InviteInfo is consumed only internally by UserManagementPage. Re-exporting both
// via `export *` would be an ambiguous-name error.
export {
  type UserInfo,
  type InviteCreated,
  listUsers,
  setUserScope,
  setUserEnabled,
  setUserPassword,
  removeUser,
  createInvite,
  listInvites,
  revokeInvite,
} from './lib/adminOps'
export * from './lib/oauthOps'
export * from './lib/domainOps'
export * from './lib/confidentialOps'
export * from './lib/nsManageOps'
export * from './lib/librarianStatus'
export * from './lib/scope'
export * from './lib/queries'
