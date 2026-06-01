import { useCallback, useState } from 'react'

// The editor's in-memory buffer state, deliberately isolated in one hook.
//
// Three structural seams the session-3 directive (§1.2) requires be left open:
//  - Draft persistence (§1.2.2): all buffer state lives here, so a future
//    `usePersistedBuffer` can wrap or replace this hook to mirror it into
//    localStorage on change — no caller restructuring.
//  - Path-less new files (§1.2.3): the buffer does not take a path. Dirty
//    tracking, content, and load/save transitions work whether or not a path
//    exists, so a future "edit then choose a path at save time" flow reuses it.
//  - The buffer is NEVER replaced except through an explicit user action that
//    routes through `load`/`markSaved` (§2 no-silent-overwrite).
export interface EditorBuffer {
  /** Current editor content. */
  content: string
  /** Replace the current content (a keystroke / programmatic edit). */
  setContent: (next: string) => void
  /** The content as last loaded or saved — the dirty comparison baseline. */
  baseline: string
  /** The etag to send as the next SAVE_FILE `if_match`. */
  etag: string
  /** content !== baseline. */
  dirty: boolean
  /** True once the initial server content has been loaded into the buffer. */
  initialized: boolean
  /**
   * Load freshly-read server content into the buffer (initial open, or
   * "Discard mine, load latest"): content and baseline both become `content`,
   * so the buffer is clean and tracks against the new server state.
   */
  load: (content: string, etag: string) => void
  /**
   * Record a successful save: the just-saved content becomes the new baseline
   * (clearing dirty) and `etag` becomes the if_match for the next save.
   */
  markSaved: (content: string, etag: string) => void
}

export function useEditorBuffer(): EditorBuffer {
  const [content, setContent] = useState('')
  const [baseline, setBaseline] = useState('')
  const [etag, setEtag] = useState('')
  const [initialized, setInitialized] = useState(false)

  const load = useCallback((next: string, nextEtag: string) => {
    setContent(next)
    setBaseline(next)
    setEtag(nextEtag)
    setInitialized(true)
  }, [])

  const markSaved = useCallback((saved: string, nextEtag: string) => {
    setBaseline(saved)
    setEtag(nextEtag)
  }, [])

  return {
    content,
    setContent,
    baseline,
    etag,
    dirty: content !== baseline,
    initialized,
    load,
    markSaved,
  }
}
