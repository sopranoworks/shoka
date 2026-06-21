import { describe, it, expect } from 'vitest'
import { visibleSettingsItems } from './settingsRegistry'

// B-28 stage 3 + part 2: the settings item list is permission-filtered. User management +
// OAuth connections are super-user-only; the namespace/project-management item is visible
// to a super-user OR any namespace-admin; My Account is visible to EVERY authenticated user
// (the gear/Settings view itself is always shown).
describe('visibleSettingsItems', () => {
  it('shows the super-user items + namespaces + account to a super-user', () => {
    const ids = visibleSettingsItems({ isSuperUser: true, managesAnyNamespace: true }).map((i) => i.id)
    expect(ids).toContain('account')
    expect(ids).toContain('users')
    expect(ids).toContain('oauth')
    expect(ids).toContain('namespaces')
  })

  it('hides the super-user items from a non-super-user but ALWAYS shows My Account', () => {
    const ids = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: false }).map((i) => i.id)
    expect(ids).toContain('account')
    expect(ids).not.toContain('users')
    expect(ids).not.toContain('oauth')
    expect(ids).not.toContain('namespaces')
  })

  it('shows My Account to everyone (a plain non-admin sees ONLY account)', () => {
    const ids = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: false }).map((i) => i.id)
    expect(ids).toEqual(['account'])
  })

  it('shows account + namespaces to a namespace-admin (not super-user)', () => {
    const ids = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: true }).map((i) => i.id)
    expect(ids).toEqual(['account', 'namespaces'])
  })
})
