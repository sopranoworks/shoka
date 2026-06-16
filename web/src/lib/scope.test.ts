import { describe, it, expect } from 'vitest'
import { parseScope, serializeScope, describeScope } from './scope'

describe('scope', () => {
  it('parses super-user from "" and "*"', () => {
    for (const s of ['', '*']) {
      expect(parseScope(s)).toEqual([{ target: '*', level: 'admin' }])
    }
  })

  it('parses namespace and wildcard grants with levels', () => {
    expect(parseScope('namespace:foo:r,namespace:bar:rw,*:r')).toEqual([
      { target: 'foo', level: 'r' },
      { target: 'bar', level: 'rw' },
      { target: '*', level: 'r' },
    ])
  })

  it('maps a legacy level-less namespace grant to read-write', () => {
    expect(parseScope('namespace:foo')).toEqual([{ target: 'foo', level: 'rw' }])
  })

  it('serializes grants back to the scope grammar', () => {
    expect(serializeScope([{ target: 'foo', level: 'rw' }, { target: '*', level: 'admin' }])).toBe(
      'namespace:foo:rw,*:admin',
    )
  })

  it('serialize keeps the most-permissive on duplicate targets', () => {
    expect(serializeScope([{ target: 'foo', level: 'r' }, { target: 'foo', level: 'admin' }])).toBe(
      'namespace:foo:admin',
    )
  })

  it('round-trips', () => {
    const s = 'namespace:foo:rw,namespace:bar:r'
    expect(serializeScope(parseScope(s))).toBe(s)
  })

  it('describes readably', () => {
    expect(describeScope('*')).toMatch(/super-user/)
    expect(describeScope('namespace:foo:rw,namespace:bar:r')).toBe('foo: read-write, bar: read-only')
  })
})
