import { describe, it, expect } from 'vitest'
import { validateFilePath } from './pathValidation'

describe('validateFilePath', () => {
  it('accepts a plain relative path', () => {
    expect(validateFilePath('notes/today.md')).toBeNull()
    expect(validateFilePath('README.md')).toBeNull()
  })

  it('rejects empty / whitespace-only', () => {
    expect(validateFilePath('')).toMatch(/file path/i)
    expect(validateFilePath('   ')).toMatch(/file path/i)
  })

  it('rejects a leading slash (must be relative)', () => {
    expect(validateFilePath('/etc/passwd')).toMatch(/relative/i)
  })

  it('rejects a trailing slash (must be a file)', () => {
    expect(validateFilePath('dir/')).toMatch(/file/i)
  })

  it('rejects empty segments', () => {
    expect(validateFilePath('a//b.md')).toMatch(/empty segment/i)
  })

  it('rejects "." and ".." segments', () => {
    expect(validateFilePath('../escape.md')).toMatch(/\.\./)
    expect(validateFilePath('a/../b.md')).toMatch(/\.\./)
    expect(validateFilePath('./a.md')).toMatch(/segments/i)
  })
})
