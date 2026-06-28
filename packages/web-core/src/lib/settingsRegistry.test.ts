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

  it('shows Server Info + My Account to everyone (a plain non-admin sees ONLY those)', () => {
    const ids = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: false }).map((i) => i.id)
    expect(ids).toEqual(['server-info', 'account'])
  })

  it('shows server-info + account + namespaces to a namespace-admin (not super-user)', () => {
    const ids = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: true }).map((i) => i.id)
    expect(ids).toEqual(['server-info', 'account', 'namespaces'])
  })

  // Extension seam (Layer-C): a consumer can inject extra items, merged AFTER the
  // built-ins and filtered by their own visibility predicate. Default (no extras) is
  // unchanged — the tests above pass `visibleSettingsItems(v)` with no second arg.
  it('merges injected extra items after the built-ins, respecting their predicate', () => {
    const extras = [
      { id: 'ssh-keys', label: 'SSH keys', visible: (v: { isSuperUser: boolean }) => v.isSuperUser },
      { id: 'always', label: 'Always', visible: () => true },
    ]
    const su = visibleSettingsItems({ isSuperUser: true, managesAnyNamespace: true }, extras).map((i) => i.id)
    expect(su).toContain('ssh-keys')
    expect(su).toContain('always')
    // built-ins still come first, extras appended in order
    expect(su.indexOf('account')).toBeLessThan(su.indexOf('ssh-keys'))

    const plain = visibleSettingsItems({ isSuperUser: false, managesAnyNamespace: false }, extras).map((i) => i.id)
    expect(plain).not.toContain('ssh-keys') // gated extra hidden from non-super-user
    expect(plain).toContain('always') // always-visible extra shown
  })
})
