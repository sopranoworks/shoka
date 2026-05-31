import { describe, it, expect } from 'vitest'
import { classifyFile } from './fileKind'

describe('classifyFile', () => {
  it('treats .md and .markdown as markdown', () => {
    expect(classifyFile('backlog.md', '# Hi')).toBe('markdown')
    expect(classifyFile('notes/x.markdown', '# Hi')).toBe('markdown')
  })

  it('treats other text files as plain text', () => {
    expect(classifyFile('main.go', 'package main')).toBe('text')
    expect(classifyFile('config.yaml', 'a: 1')).toBe('text')
    expect(classifyFile('LICENSE', 'MIT')).toBe('text')
  })

  it('treats known binary extensions as binary', () => {
    expect(classifyFile('logo.png', 'whatever')).toBe('binary')
    expect(classifyFile('doc.pdf', 'whatever')).toBe('binary')
  })

  it('treats content with a NUL byte as binary regardless of extension', () => {
    expect(classifyFile('weird.txt', 'a' + String.fromCharCode(0) + 'b')).toBe(
      'binary',
    )
  })
})
