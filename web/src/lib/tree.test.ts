import { describe, it, expect } from 'vitest'
import {
  toTreeNodes,
  flattenFilePaths,
  ancestorDirs,
  namespacesOf,
} from '@shoka/web-core'
import type { FileNode } from '@shoka/web-core'

const tree: FileNode[] = [
  {
    name: 'directives',
    path: 'directives',
    isDir: true,
    children: [
      { name: 'a.md', path: 'directives/a.md', isDir: false },
      { name: 'b.md', path: 'directives/b.md', isDir: false },
    ],
  },
  { name: 'backlog.md', path: 'backlog.md', isDir: false },
]

describe('toTreeNodes', () => {
  it('maps isDir->isFile and uses the path as id', () => {
    const out = toTreeNodes(tree)
    expect(out[0]).toMatchObject({ id: 'directives', isFile: false })
    expect(out[0].children?.[0]).toMatchObject({
      id: 'directives/a.md',
      isFile: true,
    })
    expect(out[1]).toMatchObject({ id: 'backlog.md', isFile: true })
  })

  it('leaves files without a children array', () => {
    expect(toTreeNodes(tree)[1].children).toBeUndefined()
  })
})

describe('flattenFilePaths', () => {
  it('returns only file paths, depth-first, excluding directories', () => {
    expect(flattenFilePaths(tree)).toEqual([
      'directives/a.md',
      'directives/b.md',
      'backlog.md',
    ])
  })
})

describe('ancestorDirs', () => {
  it('returns each ancestor directory of a deep path', () => {
    expect(ancestorDirs('a/b/c.md')).toEqual(['a', 'a/b'])
  })

  it('returns nothing for a top-level file', () => {
    expect(ancestorDirs('backlog.md')).toEqual([])
  })
})

describe('namespacesOf', () => {
  it('returns unique namespaces, sorted', () => {
    expect(
      namespacesOf([
        { namespace: 'shoka', name: 'maintenance', state: 'healthy' },
        { namespace: 'demo', name: 'docs', state: 'healthy' },
        { namespace: 'shoka', name: 'design', state: 'healthy' },
      ]),
    ).toEqual(['demo', 'shoka'])
  })
})
