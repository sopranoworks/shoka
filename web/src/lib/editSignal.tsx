import {
  createContext,
  useCallback,
  useContext,
  useState,
  type ReactNode,
} from 'react'

// An external-change signal for the edit route. Session 2's displayed-core
// mechanism couples (route → query key → refetch); the editor's "core" is the
// in-memory buffer, which must NOT be refetch-replaced. So instead of routing an
// edit-route NOTIFY into the generic banner (whose Reload refetches), the router
// emits a buffer-safe signal here and the editor renders its own banner with
// edit-aware actions (Resolve now / Save mine as new / Discard). This is the
// seam that lets the edit route reuse the NOTIFY plumbing without inheriting the
// refetch-and-replace semantics that would silently discard the user's edits.
// `write` / `delete`: an external change to the file under edit → the editor
// renders its own buffer-safe banner. `move`: the file under edit was moved by
// another connection → the editor itself performs a buffer-safe follow to `to`
// (bypassing its own unsaved-changes guard so a dirty buffer rides along). The
// move case is routed through the editor — not navigated by NotifyBridge — so it
// is not blocked by the editor's useBlocker dirty guard.
export type EditSignal =
  | { kind: 'write'; path: string }
  | { kind: 'delete'; path: string }
  | { kind: 'move'; path: string; to: string }

interface EditSignalCtx {
  signal: EditSignal | null
  set: (s: EditSignal) => void
  clear: () => void
}

const Ctx = createContext<EditSignalCtx | null>(null)

export function EditSignalProvider({ children }: { children: ReactNode }) {
  const [signal, setSignal] = useState<EditSignal | null>(null)
  const set = useCallback((s: EditSignal) => setSignal(s), [])
  const clear = useCallback(() => setSignal(null), [])
  return <Ctx.Provider value={{ signal, set, clear }}>{children}</Ctx.Provider>
}

export function useEditSignal(): EditSignalCtx {
  const ctx = useContext(Ctx)
  if (!ctx) throw new Error('useEditSignal must be used within EditSignalProvider')
  return ctx
}
