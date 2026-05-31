import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from 'react'

interface PaletteCtx {
  open: boolean
  // Optional mode preselects the quick-open page when launched via Cmd+P.
  mode: 'commands' | 'files'
  setOpen: (open: boolean) => void
  setMode: (mode: 'commands' | 'files') => void
  openPalette: (mode?: 'commands' | 'files') => void
  closePalette: () => void
}

const Ctx = createContext<PaletteCtx | null>(null)

export function PaletteProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false)
  const [mode, setMode] = useState<'commands' | 'files'>('commands')

  const openPalette = useCallback((m: 'commands' | 'files' = 'commands') => {
    setMode(m)
    setOpen(true)
  }, [])

  const closePalette = useCallback(() => setOpen(false), [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const meta = e.metaKey || e.ctrlKey
      // Cmd/Ctrl+K  or  Shift+Cmd/Ctrl+P -> command palette
      if (meta && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault()
        openPalette('commands')
        return
      }
      if (meta && e.shiftKey && (e.key === 'p' || e.key === 'P')) {
        e.preventDefault()
        openPalette('commands')
        return
      }
      // Cmd/Ctrl+P (no shift) -> quick-open files
      if (meta && !e.shiftKey && (e.key === 'p' || e.key === 'P')) {
        e.preventDefault()
        openPalette('files')
        return
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [openPalette])

  return (
    <Ctx.Provider
      value={{ open, mode, setOpen, setMode, openPalette, closePalette }}
    >
      {children}
    </Ctx.Provider>
  )
}

export function usePalette(): PaletteCtx {
  const ctx = useContext(Ctx)
  if (!ctx) throw new Error('usePalette must be used within PaletteProvider')
  return ctx
}
