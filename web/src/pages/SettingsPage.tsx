import { useRouterState } from '@tanstack/react-router'
import { UserManagementPage } from './UserManagementPage'
import { useIsSuperUser } from '../lib/authStatus'
import styles from './SettingsPage.module.css'

// SettingsPage is the right-pane content of the Settings rail mode (B-28 stage 3). It
// reads the selected item from the URL (`?item=`) — the sidebar's SettingsItemList
// drives it — and renders that item's screen. Permission is re-checked here (not just
// hidden in the list): a non-super-user reaching the user-management item sees a
// forbidden notice (the authoritative gate is server-side on each ADMIN_* op).
export function SettingsPage() {
  const item = useRouterState({ select: (s) => (s.location.search as { item?: string }).item })
  const isSuperUser = useIsSuperUser()

  if (!item) {
    return (
      <div className={styles.placeholder}>
        <h1 className={styles.title}>Settings</h1>
        <p>Choose a setting from the list.</p>
      </div>
    )
  }

  if (item === 'users') {
    if (!isSuperUser) {
      return (
        <div className={styles.placeholder}>
          <h1 className={styles.title}>User management</h1>
          <p>You do not have permission to manage users.</p>
        </div>
      )
    }
    return <UserManagementPage />
  }

  return (
    <div className={styles.placeholder}>
      <h1 className={styles.title}>Settings</h1>
      <p>Unknown settings item.</p>
    </div>
  )
}
