import { useEffect, useId, useState } from 'react'
import styles from './MermaidDiagram.module.css'

// Lazy singleton: import + initialize mermaid (v11, MIT) once, on the FIRST diagram only —
// mermaid is large (~loaded on demand), so a dynamic import keeps it out of the main bundle.
// startOnLoad:false because we render each block explicitly (the SPA renders markdown
// dynamically — a one-time page scan would miss navigations). securityLevel:'strict' makes
// mermaid sanitize its SVG output (DOMPurify), so injecting it is safe and CSP-clean (mermaid
// uses no eval).
let mermaidPromise: Promise<typeof import('mermaid').default> | null = null
function loadMermaid() {
  if (!mermaidPromise) {
    mermaidPromise = import('mermaid').then((mod) => {
      mod.default.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'default' })
      return mod.default
    })
  }
  return mermaidPromise
}

// MermaidDiagram renders one ```mermaid fence as an SVG diagram. It is GRACEFUL per block: a
// parse/render error shows an inline error + the raw source as a fallback, and NEVER throws —
// so a malformed diagram can't crash or blank the surrounding document. It re-renders when the
// `code` changes (file navigation / live /ws/ui update), clearing the prior SVG first so there
// is no stale or double render.
export function MermaidDiagram({ code }: { code: string }) {
  // mermaid needs a unique, DOM-valid id per render; useId is stable per instance (colons are
  // not valid in the mermaid id, so strip them).
  const id = 'mermaid-' + useId().replace(/:/g, '')
  const [svg, setSvg] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let cancelled = false
    setSvg(null)
    setFailed(false)
    loadMermaid()
      .then(async (mermaid) => {
        // Validate first (suppressErrors → returns false instead of throwing), so an invalid
        // block does not leave an orphaned mermaid error node in the DOM.
        const valid = await mermaid.parse(code, { suppressErrors: true })
        if (valid === false) throw new Error('invalid mermaid diagram')
        const { svg } = await mermaid.render(id, code)
        if (!cancelled) setSvg(svg)
      })
      .catch(() => {
        if (!cancelled) setFailed(true)
      })
    return () => {
      cancelled = true
    }
  }, [code, id])

  if (failed) {
    return (
      <div className={styles.error} role="alert" data-testid="mermaid-error">
        <span className={styles.errorLabel}>Diagram could not be rendered</span>
        <pre className={styles.raw}>{code}</pre>
      </div>
    )
  }
  if (svg) {
    return (
      <div
        className={styles.diagram}
        data-testid="mermaid-diagram"
        // mermaid sanitizes the SVG under securityLevel:'strict' before returning it.
        dangerouslySetInnerHTML={{ __html: svg }}
      />
    )
  }
  return (
    <div className={styles.loading} data-testid="mermaid-loading">
      Rendering diagram…
    </div>
  )
}
