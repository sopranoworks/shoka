import { describe, it, expect } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useEditorBuffer } from './useEditorBuffer'

describe('useEditorBuffer', () => {
  it('starts uninitialized and clean', () => {
    const { result } = renderHook(() => useEditorBuffer())
    expect(result.current.initialized).toBe(false)
    expect(result.current.dirty).toBe(false)
  })

  it('load() seeds content + baseline + etag and is clean', () => {
    const { result } = renderHook(() => useEditorBuffer())
    act(() => result.current.load('hello', 'etag-1'))
    expect(result.current.initialized).toBe(true)
    expect(result.current.content).toBe('hello')
    expect(result.current.etag).toBe('etag-1')
    expect(result.current.dirty).toBe(false)
  })

  it('becomes dirty when content diverges from baseline, clean when it matches again', () => {
    const { result } = renderHook(() => useEditorBuffer())
    act(() => result.current.load('hello', 'e1'))
    act(() => result.current.setContent('hello world'))
    expect(result.current.dirty).toBe(true)
    act(() => result.current.setContent('hello'))
    expect(result.current.dirty).toBe(false)
  })

  it('markSaved() rebaselines to the saved content and advances the etag (clears dirty)', () => {
    const { result } = renderHook(() => useEditorBuffer())
    act(() => result.current.load('hello', 'e1'))
    act(() => result.current.setContent('hello world'))
    act(() => result.current.markSaved('hello world', 'e2'))
    expect(result.current.dirty).toBe(false)
    expect(result.current.etag).toBe('e2')
    expect(result.current.baseline).toBe('hello world')
  })

  it('load() again (e.g. Discard mine) replaces the buffer and resets the etag', () => {
    const { result } = renderHook(() => useEditorBuffer())
    act(() => result.current.load('mine', 'e1'))
    act(() => result.current.setContent('my edits'))
    act(() => result.current.load('server latest', 'e9'))
    expect(result.current.content).toBe('server latest')
    expect(result.current.etag).toBe('e9')
    expect(result.current.dirty).toBe(false)
  })
})
