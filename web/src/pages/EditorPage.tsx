import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import CodeMirror from '@uiw/react-codemirror'
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels'
import { useNavigate, useBlocker } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { editRoute } from '../router'
import {
  useFileQuery,
  useEditSignal,
  useMediaQuery,
  useTheme,
  languageForPath,
  useDebouncedValue,
  classifyFile,
  Markdown,
  DiffView,
  PromptDialog,
} from '@shoka/web-core'
import { useEditorBuffer } from '../lib/useEditorBuffer'
import { saveFile, readFileFresh, fileExists } from '../lib/fileOps'
import { ConflictBanner } from '../components/ConflictBanner'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { ExternalChangeBanner } from '../components/ExternalChangeBanner'
import styles from './EditorPage.module.css'

const PREVIEW_DEBOUNCE_MS = 200

interface ConflictState {
  // The path the conflict pertains to — usually the route path, but the saved-
  // as target after a "Save as" into an existing, concurrently-modified file.
  target: string
  currentEtag: string
  message: string
}

// Editor for an existing file (session 3). Loads the file over /ws/ui READ_FILE
// (which returns {path, content, etag}), copies the content into an in-memory
// buffer, and keeps the etag for the eventual save's if_match. The buffer is
// decoupled from the TanStack Query cache, so a background invalidation never
// silently replaces what the user is editing (§2).
export function EditorPage() {
  const { namespace, project, _splat } = editRoute.useParams()
  const path = _splat ?? ''
  const { data, isError } = useFileQuery(namespace, project, path)
  const { theme } = useTheme()
  const buf = useEditorBuffer()
  const { content, etag, dirty, initialized, load, markSaved, setContent } = buf
  const isNarrow = useMediaQuery('(max-width: 640px)')
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { signal: extSignal, clear: clearExtSignal } = useEditSignal()

  const [conflict, setConflict] = useState<ConflictState | null>(null)
  const [busy, setBusy] = useState(false)
  // Save-as path prompt; pending overwrite-confirm target; diff view content.
  const [saveAsOpen, setSaveAsOpen] = useState(false)
  const [overwrite, setOverwrite] = useState<{ path: string; etag: string } | null>(
    null,
  )
  const [diffServer, setDiffServer] = useState<string | null>(null)

  // Set while a deliberate post-save navigation is in flight, so the unsaved-
  // changes guard does not prompt on the editor's own redirect to the viewer.
  const bypassGuard = useRef(false)

  // One hook covers every exit path while the buffer is dirty: shouldBlockFn
  // intercepts in-app navigation (Cancel, palette, Back/Forward, view↔edit) and
  // surfaces a confirm dialog; enableBeforeUnload arms the browser's native
  // prompt for reload / tab close (§3.4, §1.4 calibre). The predicate reads
  // bypassGuard.current LIVE (not a precomputed boolean) so the bypass set just
  // before the editor's own post-save navigate() takes effect that same tick.
  const guardActive = useCallback(() => dirty && !bypassGuard.current, [dirty])
  const blocker = useBlocker({
    shouldBlockFn: guardActive,
    enableBeforeUnload: guardActive,
    withResolver: true,
  })

  const isMarkdown = useMemo(
    () => classifyFile(path, buf.baseline) === 'markdown',
    [path, buf.baseline],
  )
  const [previewOpen, setPreviewOpen] = useState(true)
  const previewSource = useDebouncedValue(content, PREVIEW_DEBOUNCE_MS)

  // Initialize the buffer once, when the file first loads.
  useEffect(() => {
    if (data && !initialized) load(data.content, data.etag)
  }, [data, initialized, load])

  // Buffer-safe follow: if the file under edit was moved by another connection,
  // the editor navigates itself to the new path WITHOUT discarding the buffer.
  // It must drive the navigation (rather than NotifyBridge) so it can set
  // bypassGuard — otherwise the unsaved-changes guard would block the follow and
  // prompt to discard. The buffer (already initialized) survives the same-route
  // param change and rides to the new path; the moved content/etag are unchanged.
  useEffect(() => {
    if (extSignal?.kind === 'move' && extSignal.path === path) {
      bypassGuard.current = true
      const to = extSignal.to
      clearExtSignal()
      void navigate({
        to: '/p/$namespace/$project/edit/$',
        params: { namespace, project, _splat: to },
      })
    }
  }, [extSignal, path, namespace, project, navigate, clearExtSignal])

  // Re-arm the unsaved-changes guard after the path settles (the move-follow
  // above set it to bypass for that one navigation).
  useEffect(() => {
    bypassGuard.current = false
  }, [path])

  // Commit a successful save: prime the file cache so the viewer shows the new
  // content immediately, rebaseline the buffer (clean → no beforeunload prompt),
  // and navigate to the file view for the saved path.
  const applySaved = useCallback(
    (savedContent: string, newEtag: string, targetPath: string) => {
      queryClient.setQueryData(['file', namespace, project, targetPath], {
        path: targetPath,
        content: savedContent,
        etag: newEtag,
      })
      markSaved(savedContent, newEtag)
      setConflict(null)
      // Bypass the unsaved-changes guard for this intentional redirect (the
      // buffer is clean now, but the bypass also covers the state-flush gap).
      bypassGuard.current = true
      void navigate({
        to: '/p/$namespace/$project/blob/$',
        params: { namespace, project, _splat: targetPath },
      })
    },
    [queryClient, namespace, project, markSaved, navigate],
  )

  const withBusy = useCallback(async (fn: () => Promise<void>) => {
    setBusy(true)
    try {
      await fn()
    } finally {
      setBusy(false)
    }
  }, [])

  // The one low-level write: save the buffer to `targetPath` with `ifMatch`
  // (null = unchecked create). Success commits + navigates; a conflict raises
  // the four-button banner for that target.
  const persist = useCallback(
    async (targetPath: string, ifMatch: string | null) => {
      const res = await saveFile({
        namespace,
        project,
        path: targetPath,
        content,
        ifMatch,
      })
      if (res.ok) applySaved(content, res.etag, targetPath)
      else
        setConflict({
          target: targetPath,
          currentEtag: res.currentEtag,
          message: res.message,
        })
    },
    [namespace, project, content, applySaved],
  )

  // Save: optimistic write to the route path with the buffer's etag.
  const handleSave = useCallback(
    () => void withBusy(() => persist(path, etag)),
    [withBusy, persist, path, etag],
  )

  // Force overwrite: save against the server's current etag (from the CONFLICT
  // frame), overwriting whatever the other writer did. Gated by the banner's
  // explicit confirm.
  const forceOverwrite = useCallback(() => {
    if (conflict) void withBusy(() => persist(conflict.target, conflict.currentEtag))
  }, [conflict, withBusy, persist])

  // Discard mine, load latest: re-read the route file's server content and
  // replace the buffer (the user's choice — explicit, not silent).
  const discardYours = useCallback(
    () =>
      void withBusy(async () => {
        const fresh = await readFileFresh(namespace, project, path)
        load(fresh.content, fresh.etag)
        setConflict(null)
      }),
    [withBusy, namespace, project, path, load],
  )

  // Save as: write the buffer to a user-chosen path. A new path is created
  // unchecked; an existing path goes through an overwrite confirm, which then
  // saves against that path's current etag (a fresh conflict there reopens the
  // banner for the new target — recursive but structurally bounded).
  const saveAsConfirm = useCallback(
    (newPath: string) => {
      setSaveAsOpen(false)
      void withBusy(async () => {
        const ex = await fileExists(namespace, project, newPath)
        if (!ex.exists) await persist(newPath, null)
        else setOverwrite({ path: newPath, etag: ex.etag ?? '' })
      })
    },
    [withBusy, namespace, project, persist],
  )

  const overwriteConfirm = useCallback(() => {
    if (!overwrite) return
    const target = overwrite
    setOverwrite(null)
    void withBusy(() => persist(target.path, target.etag))
  }, [overwrite, withBusy, persist])

  // Show diff: fetch the conflict target's server-latest content and open the
  // diff view (server vs buffer).
  const showDiff = useCallback(() => {
    const target = conflict?.target ?? path
    void withBusy(async () => {
      const fresh = await readFileFresh(namespace, project, target)
      setDiffServer(fresh.content)
    })
  }, [conflict, path, withBusy, namespace, project])

  // External-change banner actions (edit route, from the editSignal). None
  // touch the buffer except the user's explicit choice.
  //  - write "Resolve now": fetch the server's current etag and open the same
  //    four-button conflict UX a save conflict would.
  const resolveExternal = useCallback(() => {
    clearExtSignal()
    void withBusy(async () => {
      const fresh = await readFileFresh(namespace, project, path)
      setConflict({
        target: path,
        currentEtag: fresh.etag,
        message: 'This file was modified by someone else.',
      })
    })
  }, [clearExtSignal, withBusy, namespace, project, path])

  //  - delete "Save mine as new file": the Save-as dialog (defaulting to the
  //    deleted path, which a no-if_match write recreates — precursor §7).
  const saveMineAsNew = useCallback(() => {
    clearExtSignal()
    setSaveAsOpen(true)
  }, [clearExtSignal])

  //  - delete "Discard mine, go to tree": abandon the buffer, go to the project.
  const discardToTree = useCallback(() => {
    clearExtSignal()
    bypassGuard.current = true
    void navigate({
      to: '/p/$namespace/$project',
      params: { namespace, project },
    })
  }, [clearExtSignal, navigate, namespace, project])

  // Cancel returns to the file view. When the buffer is dirty the navigation is
  // intercepted by the guard above (which raises the confirm dialog); when clean
  // it navigates straight through.
  const goToView = useCallback(() => {
    void navigate({
      to: '/p/$namespace/$project/blob/$',
      params: { namespace, project, _splat: path },
    })
  }, [navigate, namespace, project, path])

  // Language extension by file extension (session 4): markdown gets
  // @codemirror/lang-markdown back; code files get their language. The packages
  // travel in the lazy editor chunk, not the initial bundle.
  const langExtensions = useMemo(() => languageForPath(path), [path])

  const cm = (
    <CodeMirror
      value={content}
      onChange={setContent}
      theme={theme === 'dark' ? 'dark' : 'light'}
      extensions={langExtensions}
      height="100%"
      className={styles.editor}
    />
  )

  const showSplit = isMarkdown && previewOpen

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath} title={path}>
          {path}
        </span>
        <div className={styles.toolbarActions}>
          {isMarkdown && (
            <button
              className={styles.ghostBtn}
              onClick={() => setPreviewOpen((v) => !v)}
            >
              {previewOpen ? 'Hide preview' : 'Show preview'}
            </button>
          )}
          <button
            className={styles.ghostBtn}
            disabled={busy}
            onClick={goToView}
          >
            Cancel
          </button>
          <button
            className={styles.saveBtn}
            disabled={!dirty || busy || isError}
            onClick={handleSave}
          >
            Save
          </button>
        </div>
      </div>

      {conflict ? (
        <ConflictBanner
          message={conflict.message}
          busy={busy}
          onDiscardYours={discardYours}
          onForceOverwrite={forceOverwrite}
          onSaveAs={() => setSaveAsOpen(true)}
          onShowDiff={showDiff}
        />
      ) : extSignal && extSignal.kind !== 'move' && extSignal.path === path ? (
        <ExternalChangeBanner
          kind={extSignal.kind}
          busy={busy}
          onResolve={resolveExternal}
          onSaveAsNew={saveMineAsNew}
          onDiscardToTree={discardToTree}
          onDismiss={clearExtSignal}
        />
      ) : null}

      <div className={styles.body}>
        {isError ? (
          <div className={styles.error}>
            File not found: <code>{path}</code>
          </div>
        ) : !initialized ? (
          <div className={styles.loading}>Loading…</div>
        ) : showSplit ? (
          <PanelGroup
            key={isNarrow ? 'v' : 'h'}
            direction={isNarrow ? 'vertical' : 'horizontal'}
            autoSaveId={isNarrow ? undefined : 'shoka-editor-split'}
            className={styles.split}
          >
            <Panel id="source" order={1} minSize={20} className={styles.sourcePanel}>
              {cm}
            </Panel>
            <PanelResizeHandle
              className={isNarrow ? styles.resizeHandleV : styles.resizeHandle}
            />
            <Panel
              id="preview"
              order={2}
              minSize={20}
              className={styles.previewPanel}
            >
              <div className={styles.previewHead}>
                <span>Preview</span>
                <button
                  className={styles.closePreview}
                  title="Close preview"
                  aria-label="Close preview"
                  onClick={() => setPreviewOpen(false)}
                >
                  ×
                </button>
              </div>
              <div className={styles.previewBody}>
                <Markdown content={previewSource} />
              </div>
            </Panel>
          </PanelGroup>
        ) : (
          <div className={styles.singlePane}>{cm}</div>
        )}
      </div>

      <ConfirmDialog
        open={blocker.status === 'blocked'}
        title="Discard unsaved changes?"
        message="You have unsaved edits in this file. Leaving the editor will discard them."
        confirmLabel="Discard changes"
        cancelLabel="Keep editing"
        danger
        onConfirm={() => blocker.proceed?.()}
        onCancel={() => blocker.reset?.()}
      />

      <PromptDialog
        open={saveAsOpen}
        title="Save as"
        label="Save to path"
        defaultValue={conflict?.target ?? path}
        confirmLabel="Save"
        onConfirm={saveAsConfirm}
        onCancel={() => setSaveAsOpen(false)}
      />

      <ConfirmDialog
        open={overwrite !== null}
        title="Overwrite existing file?"
        message={`File ${overwrite?.path ?? ''} already exists. Overwrite it with your content?`}
        confirmLabel="Overwrite"
        cancelLabel="Cancel"
        danger
        onConfirm={overwriteConfirm}
        onCancel={() => setOverwrite(null)}
      />

      <DiffView
        open={diffServer !== null}
        serverContent={diffServer ?? ''}
        bufferContent={content}
        onClose={() => setDiffServer(null)}
      />
    </div>
  )
}
