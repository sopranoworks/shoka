import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useDebouncedValue } from './useDebouncedValue'

describe('useDebouncedValue', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('returns the initial value immediately', () => {
    const { result } = renderHook(() => useDebouncedValue('a', 200))
    expect(result.current).toBe('a')
  })

  it('updates only after the delay elapses since the last change', () => {
    const { result, rerender } = renderHook(
      ({ v }) => useDebouncedValue(v, 200),
      { initialProps: { v: 'a' } },
    )

    rerender({ v: 'b' })
    // Not yet — only 199ms passed.
    act(() => void vi.advanceTimersByTime(199))
    expect(result.current).toBe('a')

    act(() => void vi.advanceTimersByTime(1))
    expect(result.current).toBe('b')
  })

  it('coalesces a burst: only the last value survives the debounce window', () => {
    const { result, rerender } = renderHook(
      ({ v }) => useDebouncedValue(v, 200),
      { initialProps: { v: '' } },
    )

    rerender({ v: 'h' })
    act(() => void vi.advanceTimersByTime(50))
    rerender({ v: 'he' })
    act(() => void vi.advanceTimersByTime(50))
    rerender({ v: 'hel' })
    // The window keeps resetting; nothing has committed yet.
    expect(result.current).toBe('')

    act(() => void vi.advanceTimersByTime(200))
    expect(result.current).toBe('hel')
  })
})
