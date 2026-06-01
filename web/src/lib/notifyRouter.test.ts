import { describe, it, expect } from 'vitest'
import type { QueryClient } from '@tanstack/react-query'
import {
  routeNotify,
  routeReconnect,
  parseNotifyEvent,
  splitTarget,
  type ViewContext,
} from './notifyRouter'

interface Recorded {
  invalidate: { queryKey?: unknown[]; refetchType?: string }[]
  refetch: { queryKey?: unknown[]; type?: string }[]
  qc: QueryClient
}
function makeQc(): Recorded {
  const invalidate: Recorded['invalidate'] = []
  const refetch: Recorded['refetch'] = []
  const qc = {
    invalidateQueries: (o: unknown) => {
      invalidate.push((o ?? {}) as never)
    },
    refetchQueries: (o: unknown) => {
      refetch.push((o ?? {}) as never)
      return Promise.resolve()
    },
  } as unknown as QueryClient
  return { invalidate, refetch, qc }
}

const has = (
  list: { queryKey?: unknown[]; refetchType?: string }[],
  key: unknown[],
) => list.find((e) => JSON.stringify(e.queryKey) === JSON.stringify(key))

const BLOB: ViewContext = {
  route: 'blob',
  namespace: 'demo',
  project: 'docs',
  path: 'README.md',
}
const PROJECT: ViewContext = {
  route: 'project',
  namespace: 'demo',
  project: 'docs',
}
const HOME: ViewContext = { route: 'home' }

describe('splitTarget / parseNotifyEvent', () => {
  it('splits a joined target on the first slash', () => {
    expect(splitTarget('shoka/maintenance')).toEqual({
      namespace: 'shoka',
      project: 'maintenance',
    })
  })
  it('parses a valid event and rejects malformed payloads', () => {
    expect(
      parseNotifyEvent({ kind: 'file.write', target: 'demo/docs', path: 'a.md' }),
    ).toMatchObject({ kind: 'file.write', target: 'demo/docs', path: 'a.md' })
    expect(parseNotifyEvent({ kind: 'file.write' })).toBeNull()
    expect(parseNotifyEvent(null)).toBeNull()
  })
})

describe('routeNotify file.write', () => {
  it('on the displayed file: file key stale (no refetch) + banner; tree peripheral', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'file.write', target: 'demo/docs', path: 'README.md' },
      qc,
      BLOB,
    )
    expect(has(invalidate, ['file', 'demo', 'docs', 'README.md'])?.refetchType).toBe(
      'none',
    )
    expect(has(invalidate, ['tree', 'demo', 'docs'])?.refetchType).toBeUndefined()
    expect(r.banner?.text).toBe('This file was updated')
  })

  it('on the displayed project: tree stale (no refetch) + banner', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'file.write', target: 'demo/docs', path: 'guides/x.md' },
      qc,
      PROJECT,
    )
    expect(has(invalidate, ['tree', 'demo', 'docs'])?.refetchType).toBe('none')
    expect(r.banner?.text).toBe('Files in this project changed')
  })

  it('on an unrelated view: silent invalidate, no banner', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'file.write', target: 'other/proj', path: 'a.md' },
      qc,
      BLOB,
    )
    expect(has(invalidate, ['tree', 'other', 'proj'])?.refetchType).toBeUndefined()
    expect(r.banner).toBeUndefined()
  })
})

describe('routeNotify file.delete / project.create / catalog', () => {
  it('file.delete on displayed file shows the delete banner', () => {
    const { qc } = makeQc()
    const r = routeNotify(
      { kind: 'file.delete', target: 'demo/docs', path: 'README.md' },
      qc,
      BLOB,
    )
    expect(r.banner?.text).toBe('This file was deleted')
  })

  it('project.create on home: projects stale (no refetch) + banner', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'project.create', target: 'demo/new', path: '' },
      qc,
      HOME,
    )
    expect(has(invalidate, ['projects'])?.refetchType).toBe('none')
    expect(r.banner?.text).toBe('Projects changed')
  })

  it('project.create off home: silent invalidate, no banner', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'project.create', target: 'demo/new', path: '' },
      qc,
      BLOB,
    )
    expect(has(invalidate, ['projects'])?.refetchType).toBeUndefined()
    expect(r.banner).toBeUndefined()
  })

  it('catalog.invariant_violation: warning toast, no invalidation', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify(
      { kind: 'catalog.invariant_violation', target: 'demo/docs', path: 'a.md' },
      qc,
      BLOB,
    )
    expect(invalidate.length).toBe(0)
    expect(r.toast?.level).toBe('warn')
    expect(r.toast?.text).toContain('demo/docs')
  })

  it('unknown kind: no effects, no throw', () => {
    const { qc, invalidate } = makeQc()
    const r = routeNotify({ kind: 'weird.event', target: 'x/y' }, qc, HOME)
    expect(invalidate.length).toBe(0)
    expect(r).toEqual({})
  })
})

describe('routeReconnect', () => {
  it('marks all stale, refetches active non-core, and banners the core', () => {
    const { qc, invalidate, refetch } = makeQc()
    const r = routeReconnect(qc, BLOB)
    // whole-tree invalidate with no auto-refetch
    expect(invalidate.some((e) => e.refetchType === 'none' && !e.queryKey)).toBe(
      true,
    )
    // an active refetch excluding the core
    expect(refetch.some((e) => e.type === 'active')).toBe(true)
    expect(r.banner?.text).toMatch(/Reconnected/)
  })
})
