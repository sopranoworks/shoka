import { describe, it, expect } from 'vitest'
import {
  dirOf,
  baseOf,
  joinPath,
  movePrefill,
  computeMoveTarget,
  validateMoveInput,
} from './moveController'

describe('path helpers', () => {
  it('dirOf / baseOf split on the last slash', () => {
    expect(dirOf('a/b/c.md')).toBe('a/b')
    expect(baseOf('a/b/c.md')).toBe('c.md')
    expect(dirOf('top.md')).toBe('')
    expect(baseOf('top.md')).toBe('top.md')
  })
  it('joinPath omits the leading slash at the root', () => {
    expect(joinPath('a/b', 'c.md')).toBe('a/b/c.md')
    expect(joinPath('', 'c.md')).toBe('c.md')
  })
})

describe('movePrefill', () => {
  it('seeds the full path for Move, the basename for Rename', () => {
    expect(movePrefill('move', 'docs/guide.md')).toBe('docs/guide.md')
    expect(movePrefill('rename', 'docs/guide.md')).toBe('guide.md')
  })
})

describe('computeMoveTarget', () => {
  it('Move uses the typed path verbatim (trimmed)', () => {
    expect(computeMoveTarget('move', 'docs/a.md', '  notes/b.md ')).toBe('notes/b.md')
  })
  it('Rename keeps the source directory and swaps the basename', () => {
    expect(computeMoveTarget('rename', 'docs/a.md', 'b.md')).toBe('docs/b.md')
    expect(computeMoveTarget('rename', 'top.md', 'renamed.md')).toBe('renamed.md')
  })
})

describe('validateMoveInput', () => {
  it('accepts a valid Move target and Rename name', () => {
    expect(validateMoveInput('move', 'docs/a.md', 'notes/b.md')).toBeNull()
    expect(validateMoveInput('rename', 'docs/a.md', 'b.md')).toBeNull()
  })
  it('rejects a slash in a Rename (directory is fixed)', () => {
    expect(validateMoveInput('rename', 'docs/a.md', 'sub/b.md')).toMatch(/cannot contain/)
  })
  it('rejects a malformed / project-escaping target (delegates to validateFilePath)', () => {
    expect(validateMoveInput('move', 'docs/a.md', '/abs.md')).toMatch(/relative/)
    expect(validateMoveInput('move', 'docs/a.md', '../escape.md')).toMatch(/"\."/)
    expect(validateMoveInput('move', 'docs/a.md', '')).toMatch(/Enter a file path/)
  })
  it('reports a no-op (same path / same name) instead of silently accepting it', () => {
    expect(validateMoveInput('move', 'docs/a.md', 'docs/a.md')).toMatch(/current path/)
    expect(validateMoveInput('rename', 'docs/a.md', 'a.md')).toMatch(/current name/)
  })
})
