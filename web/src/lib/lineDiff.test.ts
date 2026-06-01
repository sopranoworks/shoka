import { describe, it, expect } from 'vitest'
import { lineDiff } from './lineDiff'

describe('lineDiff', () => {
  it('all-same content yields only same rows', () => {
    const rows = lineDiff('a\nb\nc', 'a\nb\nc')
    expect(rows.every((r) => r.type === 'same')).toBe(true)
    expect(rows.map((r) => r.text)).toEqual(['a', 'b', 'c'])
  })

  it('marks a server-only line as del and a buffer-only line as add', () => {
    // server: a, b, c ; buffer: a, x, c  → b removed, x added
    const rows = lineDiff('a\nb\nc', 'a\nx\nc')
    expect(rows).toEqual([
      { type: 'same', text: 'a' },
      { type: 'del', text: 'b' },
      { type: 'add', text: 'x' },
      { type: 'same', text: 'c' },
    ])
  })

  it('handles pure additions at the end', () => {
    const rows = lineDiff('a', 'a\nb')
    expect(rows).toEqual([
      { type: 'same', text: 'a' },
      { type: 'add', text: 'b' },
    ])
  })

  it('handles pure deletions', () => {
    const rows = lineDiff('a\nb', 'a')
    expect(rows).toEqual([
      { type: 'same', text: 'a' },
      { type: 'del', text: 'b' },
    ])
  })
})
