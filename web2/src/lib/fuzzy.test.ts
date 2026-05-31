import { describe, it, expect } from 'vitest'
import { fuzzyScore, fuzzyFilter } from './fuzzy'

describe('fuzzyScore', () => {
  it('returns 0 for an empty query', () => {
    expect(fuzzyScore('', 'anything')).toBe(0)
  })

  it('returns null when the query is not a subsequence', () => {
    expect(fuzzyScore('xyz', 'readme.md')).toBeNull()
  })

  it('matches a subsequence case-insensitively', () => {
    expect(fuzzyScore('RM', 'readme.md')).not.toBeNull()
  })

  it('scores a closer (more consecutive) match higher', () => {
    const consecutive = fuzzyScore('read', 'readme.md')
    const scattered = fuzzyScore('rdme', 'readme.md')
    expect(consecutive).not.toBeNull()
    expect(scattered).not.toBeNull()
    expect(consecutive! > scattered!).toBe(true)
  })
})

describe('fuzzyFilter', () => {
  const files = [
    'directives/2026-06-01-session-1.md',
    'reports/2026-06-01-complete.md',
    'backlog.md',
    'journal.md',
  ]

  it('drops non-matches and ranks best first', () => {
    const out = fuzzyFilter('backlog', files, (f) => f)
    expect(out.length).toBe(1)
    expect(out[0].item).toBe('backlog.md')
  })

  it('ranks an exact-ish filename above an incidental subsequence', () => {
    const out = fuzzyFilter('journal', files, (f) => f)
    expect(out[0].item).toBe('journal.md')
  })
})
