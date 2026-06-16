import { describe, it, expect } from 'vitest'
import { visibleSettingsItems } from './settingsRegistry'

// B-28 stage 3: the settings item list is permission-filtered — the user-management
// item is super-user-only, so a non-super-user's list omits it (the gear/Settings view
// itself is always shown; only the items inside are filtered).
describe('visibleSettingsItems', () => {
  it('shows the super-user items (user management + OAuth connections) to a super-user', () => {
    const ids = visibleSettingsItems(true).map((i) => i.id)
    expect(ids).toContain('users')
    expect(ids).toContain('oauth')
  })

  it('hides the super-user items from a non-super-user', () => {
    const ids = visibleSettingsItems(false).map((i) => i.id)
    expect(ids).not.toContain('users')
    expect(ids).not.toContain('oauth')
  })
})
