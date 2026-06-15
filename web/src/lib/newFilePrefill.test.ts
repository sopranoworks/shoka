import { describe, it, expect } from 'vitest'
import { newFilePrefill } from './newFilePrefill'

// The Save-path dialog seed for the new-file flow (B-31 #3/#4): a file view hands
// in its directory (→ `subdir/`, sibling-ready), the project root hands in nothing
// (→ empty). The result is always editable to any nested path downstream.
describe('newFilePrefill', () => {
  it('seeds `dir/` from a launch directory (a file view)', () => {
    expect(newFilePrefill('subdir')).toBe('subdir/')
    expect(newFilePrefill('a/b')).toBe('a/b/')
  })

  it('seeds empty at the project root (no launch directory)', () => {
    expect(newFilePrefill('')).toBe('')
    expect(newFilePrefill(undefined)).toBe('')
  })

  it('normalises an already-trailing slash (no doubled slash)', () => {
    expect(newFilePrefill('subdir/')).toBe('subdir/')
    expect(newFilePrefill('a/b//')).toBe('a/b/')
  })
})
