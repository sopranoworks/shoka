import { useCallback, useRef, useState } from 'react'
import CodeMirror from '@uiw/react-codemirror'
import { useNavigate, useBlocker } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { newFileRoute } from '../router'
import { useEditorBuffer } from '../lib/useEditorBuffer'
import { useTheme, PromptDialog } from '@shoka/web-core'
import { saveFile, fileExists } from '../lib/fileOps'
import { validateFilePath } from '../lib/pathValidation'
import { newFilePrefill } from '../lib/newFilePrefill'
import { ConfirmDialog } from '../components/ConfirmDialog'
import styles from './EditorPage.module.css'

// Path-less new-file workflow (session 4 §3.7). The buffer (useEditorBuffer) is
// path-less by design — confirmed against source, the session-3 "half-built"
// claim held — so the editor opens empty, the user types, and the path is chosen
// at Save time. Save creates the file with no if_match (the storage precondition
// from the ws-ui-versioning precursor: a no-if_match write to a missing path
// creates it). A chosen path that already exists routes through an overwrite
// confirm. No backend change. Per the operator's standing rule, the buffer is
// browser-memory only: reloading /new yields an empty buffer (no draft restore).
export function NewFilePage() {
  const { namespace, project } = newFileRoute.useParams()
  const { in: launchedFrom } = newFileRoute.useSearch()
  // Seed the Save-path dialog with the directory the create was launched from
  // (the ?in= search): `subdir/` from a file view (cursor ready to type the
  // sibling's name), empty at the project root. Fully editable to any nested
  // path — the server creates intermediate dirs as a by-product of the write.
  const pathPrefill = newFilePrefill(launchedFrom)
  const { theme } = useTheme()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const { content, setContent, dirty, markSaved } = useEditorBuffer()
  const [busy, setBusy] = useState(false)
  const [promptOpen, setPromptOpen] = useState(false)
  const [overwrite, setOverwrite] = useState<{ path: string; etag: string } | null>(null)
  const [force, setForce] = useState<{ path: string } | null>(null)

  // Bypass the unsaved-changes guard for the editor's own post-save redirect.
  const bypassGuard = useRef(false)
  const guardActive = useCallback(() => dirty && !bypassGuard.current, [dirty])
  const blocker = useBlocker({
    shouldBlockFn: guardActive,
    enableBeforeUnload: guardActive,
    withResolver: true,
  })

  const withBusy = useCallback(async (fn: () => Promise<void>) => {
    setBusy(true)
    try {
      await fn()
    } finally {
      setBusy(false)
    }
  }, [])

  // Commit a successful save: prime the file cache, rebaseline (clean → no
  // beforeunload prompt), then navigate to the new file's blob view.
  const applySaved = useCallback(
    (savedContent: string, etag: string, targetPath: string) => {
      queryClient.setQueryData(['file', namespace, project, targetPath], {
        path: targetPath,
        content: savedContent,
        etag,
      })
      void queryClient.invalidateQueries({ queryKey: ['tree', namespace, project] })
      markSaved(savedContent, etag)
      bypassGuard.current = true
      void navigate({
        to: '/p/$namespace/$project/blob/$',
        params: { namespace, project, _splat: targetPath },
      })
    },
    [queryClient, namespace, project, markSaved, navigate],
  )

  // The one low-level write. ifMatch=null creates (or, after an explicit
  // overwrite confirm, force-overwrites); a non-null ifMatch is the optimistic
  // overwrite of an existing target. A conflict on the optimistic overwrite
  // offers a final force.
  const saveAt = useCallback(
    (targetPath: string, ifMatch: string | null) =>
      withBusy(async () => {
        const res = await saveFile({ namespace, project, path: targetPath, content, ifMatch })
        if (res.ok) applySaved(content, res.etag, targetPath)
        else setForce({ path: targetPath })
      }),
    [withBusy, namespace, project, content, applySaved],
  )

  // Save: prompt for a path, validate, then create-or-overwrite.
  const onPromptConfirm = useCallback(
    (path: string) => {
      setPromptOpen(false)
      void withBusy(async () => {
        const ex = await fileExists(namespace, project, path)
        if (!ex.exists) await saveAt(path, null)
        else setOverwrite({ path, etag: ex.etag ?? '' })
      })
    },
    [withBusy, namespace, project, saveAt],
  )

  const goToProject = useCallback(() => {
    void navigate({ to: '/p/$namespace/$project', params: { namespace, project } })
  }, [navigate, namespace, project])

  return (
    <div className={styles.page}>
      <div className={styles.toolbar}>
        <span className={styles.filePath}>New file in {namespace}/{project}</span>
        <div className={styles.toolbarActions}>
          <button className={styles.ghostBtn} disabled={busy} onClick={goToProject}>
            Cancel
          </button>
          <button
            className={styles.saveBtn}
            disabled={!dirty || busy}
            onClick={() => setPromptOpen(true)}
          >
            Save…
          </button>
        </div>
      </div>

      <div className={styles.body}>
        <div className={styles.singlePane}>
          <CodeMirror
            value={content}
            onChange={setContent}
            theme={theme === 'dark' ? 'dark' : 'light'}
            height="100%"
            className={styles.editor}
            autoFocus
          />
        </div>
      </div>

      <ConfirmDialog
        open={blocker.status === 'blocked'}
        title="Discard unsaved changes?"
        message="You have unsaved content in this new file. Leaving will discard it."
        confirmLabel="Discard"
        cancelLabel="Keep editing"
        danger
        onConfirm={() => blocker.proceed?.()}
        onCancel={() => blocker.reset?.()}
      />

      <PromptDialog
        open={promptOpen}
        title="Save new file"
        label="File path (relative to the project)"
        defaultValue={pathPrefill}
        confirmLabel="Save"
        validate={validateFilePath}
        onConfirm={onPromptConfirm}
        onCancel={() => setPromptOpen(false)}
      />

      <ConfirmDialog
        open={overwrite !== null}
        title="Overwrite existing file?"
        message={`File ${overwrite?.path ?? ''} already exists. Overwrite it with your content?`}
        confirmLabel="Overwrite"
        cancelLabel="Cancel"
        danger
        onConfirm={() => {
          if (!overwrite) return
          const target = overwrite
          setOverwrite(null)
          void saveAt(target.path, target.etag)
        }}
        onCancel={() => setOverwrite(null)}
      />

      <ConfirmDialog
        open={force !== null}
        title="File changed since you checked"
        message={`${force?.path ?? ''} was modified by someone else. Overwrite it anyway?`}
        confirmLabel="Overwrite anyway"
        cancelLabel="Cancel"
        danger
        onConfirm={() => {
          if (!force) return
          const target = force
          setForce(null)
          void saveAt(target.path, null)
        }}
        onCancel={() => setForce(null)}
      />
    </div>
  )
}

export default NewFilePage
