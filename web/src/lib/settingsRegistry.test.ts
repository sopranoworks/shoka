import { describe, it, expect } from 'vitest'
import { visibleSettingsItems } from './settingsRegistry'

// B-28 stage 3: the settings item list is permission-filtered — the user-management
// item is super-user-only, so a non-super-user's list omits it (the gear/Settings view
// itself is always shown; only the items inside are filtered).
describe('visibleSettingsItems', () => {
  it('shows user management to a super-user', () => {
    const items = visibleSettingsItems(true)
    expect(items.map((i) => i.id)).toContain('users')
  })

  it('hides user management from a non-super-user', () => {
    const items = visibleSettingsItems(false)
    expect(items.map((i) => i.id)).not.toContain('users')
  })
})
