import { useEffect, useState } from 'react'

// Returns `value` delayed by `delayMs` after the last change. Used for the
// editor's live preview (§1.1: re-render 200ms after the last keystroke) so a
// burst of typing does not re-render react-markdown on every character.
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(t)
  }, [value, delayMs])
  return debounced
}
