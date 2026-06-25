import { describe, it, expect } from 'vitest'
import { deriveViewContext } from '@shoka/web-core'

describe('deriveViewContext', () => {
  it('recognises home', () => {
    expect(deriveViewContext('/')).toEqual({ route: 'home' })
  })

  it('recognises a project route', () => {
    expect(deriveViewContext('/p/demo/docs')).toEqual({
      route: 'project',
      namespace: 'demo',
      project: 'docs',
    })
  })

  it('recognises a blob route and keeps the splat path raw', () => {
    expect(deriveViewContext('/p/demo/docs/blob/guides/intro.md')).toEqual({
      route: 'blob',
      namespace: 'demo',
      project: 'docs',
      path: 'guides/intro.md',
    })
  })

  it('recognises an edit route (so NOTIFY can target the edited file)', () => {
    expect(deriveViewContext('/p/demo/docs/edit/README.md')).toEqual({
      route: 'edit',
      namespace: 'demo',
      project: 'docs',
      path: 'README.md',
    })
  })

  it('falls back to other for unknown paths', () => {
    expect(deriveViewContext('/p/demo/docs/history/README.md')).toEqual({
      route: 'other',
    })
  })
})
