import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQueryClient } from '@tanstack/react-query'
import { moveFile } from './fileOps'
import { validateFilePath } from './pathValidation'
import { deriveViewContext } from './viewContext'
import type { FileContent } from './types'
import { PromptDialog } from '../components/PromptDialog'
import { MoveCollisionWarning } from '../components/MoveCollisionWarning'

// The single controller every move surface (dialog, palette, inline rename,
// drag-and-drop, context menu) funnels through. It owns the move/rename dialog,
// the three-action collision warning, the post-move cache relocation, and the
// post-move navigation — so no surface talks to lib/fileOps.moveFile or wsClient
// directly; they all call requestMove (open the dialog) or executeMove (move a
// known target directly, e.g. drag-drop / inline rename).
//
// A move is a PURE PATH CHANGE (B-33): there is NO link surface anywhere here —
// no pre-scan, no "N links" count, no links_rewritten display. Submitting the
// dialog goes straight to the move; the only interstitial is the collision
// warning, and only when the target is already occupied.

export type MoveMode = 'move' | 'rename'

export interface MoveRequest {
  namespace: string
  project: string
  sourcePath: string
  mode: MoveMode
}

export interface ExecuteMoveArgs {
  namespace: string
  project: string
  sourcePath: string
  targetPath: string
}

export interface MoveControllerApi {
  /** Open the move/rename dialog for a file (palette, context menu). */
  requestMove: (req: MoveRequest) => void
  /**
   * Move a file to a known target directly, with full collision handling, no
   * dialog (drag-and-drop, inline rename). A same-path target is a no-op.
   */
  executeMove: (args: ExecuteMoveArgs) => Promise<void>
}

// --- pure path + validation helpers (exported for unit testing) -------------

export function dirOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? '' : path.slice(0, i)
}
export function baseOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? path : path.slice(i + 1)
}
export function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name
}

// What the dialog field is seeded with: the full project-relative path for a
// Move (edit directory + name), just the basename for a Rename (directory fixed).
export function movePrefill(mode: MoveMode, sourcePath: string): string {
  return mode === 'rename' ? baseOf(sourcePath) : sourcePath
}

// The full project-relative target the dialog input resolves to.
export function computeMoveTarget(
  mode: MoveMode,
  sourcePath: string,
  input: string,
): string {
  const v = input.trim()
  return mode === 'rename' ? joinPath(dirOf(sourcePath), v) : v
}

// Validate the dialog input. Reuses validateFilePath on the RESOLVED target so a
// project-escaping or malformed path is rejected before any MOVE_FILE is sent. A
// same-path target is reported (not silently accepted) so the user is not
// confused by a no-op. Returns an error string to show inline, or null.
export function validateMoveInput(
  mode: MoveMode,
  sourcePath: string,
  input: string,
): string | null {
  const v = input.trim()
  if (mode === 'rename' && v.includes('/'))
    return 'A name cannot contain "/". Use Move… to change the folder.'
  const target = computeMoveTarget(mode, sourcePath, input)
  const base = validateFilePath(target)
  if (base) return base
  if (target === sourcePath)
    return mode === 'rename' ? 'That is the current name.' : 'That is the current path.'
  return null
}

// --- controller -------------------------------------------------------------

interface DialogState extends MoveRequest {
  // When reopened from the collision warning's "Save under a different name",
  // the field is prefilled with the attempted (occupied) target instead of the
  // mode default, so the user edits to a free path.
  prefill?: string
}
interface CollisionState extends ExecuteMoveArgs {
  currentEtag: string
}

const Ctx = createContext<MoveControllerApi | null>(null)

export function MoveProvider({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const viewRef = useRef(deriveViewContext(pathname))
  viewRef.current = deriveViewContext(pathname)

  const [dialog, setDialog] = useState<DialogState | null>(null)
  const [collision, setCollision] = useState<CollisionState | null>(null)
  const [busy, setBusy] = useState(false)

  // Relocate caches and follow the move for the issuing client (which is
  // sender-excluded from its own file.move NOTIFY, so it drives itself here):
  //  - move the file query cache from the old key to the new (carrying content,
  //    stamping the new etag), drop the old key, and invalidate the tree;
  //  - if the moved file is the displayed blob/edit file, navigate to its new
  //    path preserving the mode (a dirty editor buffer survives — EditorPage
  //    keeps its initialized buffer across a same-route param change).
  const applyMoved = useCallback(
    (ns: string, proj: string, src: string, dst: string, newEtag: string) => {
      const old = queryClient.getQueryData<FileContent>(['file', ns, proj, src])
      if (old) {
        queryClient.setQueryData<FileContent>(['file', ns, proj, dst], {
          path: dst,
          content: old.content,
          etag: newEtag,
        })
      }
      queryClient.removeQueries({ queryKey: ['file', ns, proj, src], exact: true })
      queryClient.invalidateQueries({ queryKey: ['tree', ns, proj] })

      const view = viewRef.current
      const onMoved =
        (view.route === 'blob' || view.route === 'edit') &&
        view.namespace === ns &&
        view.project === proj &&
        (view.path ?? '') === src
      if (onMoved) {
        void navigate({
          to:
            view.route === 'edit'
              ? '/p/$namespace/$project/edit/$'
              : '/p/$namespace/$project/blob/$',
          params: { namespace: ns, project: proj, _splat: dst },
        })
      }
    },
    [queryClient, navigate],
  )

  // The one low-level move: success relocates + navigates; an occupied-target
  // CONFLICT raises the three-action warning (never a silent overwrite).
  const doMove = useCallback(
    async (
      ns: string,
      proj: string,
      src: string,
      target: string,
      ifMatch: string | null,
    ) => {
      setBusy(true)
      try {
        const res = await moveFile({
          namespace: ns,
          project: proj,
          sourcePath: src,
          targetPath: target,
          ifMatch,
        })
        if (res.ok) {
          setCollision(null)
          applyMoved(ns, proj, res.sourcePath, res.targetPath, res.newEtag)
        } else {
          setCollision({
            namespace: ns,
            project: proj,
            sourcePath: src,
            targetPath: target,
            currentEtag: res.currentEtag,
          })
        }
      } finally {
        setBusy(false)
      }
    },
    [applyMoved],
  )

  const requestMove = useCallback((req: MoveRequest) => {
    setCollision(null)
    setDialog(req)
  }, [])

  const executeMove = useCallback(
    async (args: ExecuteMoveArgs) => {
      if (args.targetPath === args.sourcePath) return // same-path: no-op
      await doMove(args.namespace, args.project, args.sourcePath, args.targetPath, null)
    },
    [doMove],
  )

  const api = useMemo<MoveControllerApi>(
    () => ({ requestMove, executeMove }),
    [requestMove, executeMove],
  )

  const onDialogConfirm = useCallback(
    (value: string) => {
      if (!dialog) return
      const { namespace, project, sourcePath, mode } = dialog
      const target = computeMoveTarget(mode, sourcePath, value)
      setDialog(null)
      if (target === sourcePath) return // no-op (validation also blocks this)
      void doMove(namespace, project, sourcePath, target, null)
    },
    [dialog, doMove],
  )

  return (
    <Ctx.Provider value={api}>
      {children}

      {dialog && (
        <PromptDialog
          open
          title={dialog.mode === 'rename' ? 'Rename file' : 'Move file'}
          label={dialog.mode === 'rename' ? 'New name' : 'Move to path'}
          defaultValue={dialog.prefill ?? movePrefill(dialog.mode, dialog.sourcePath)}
          confirmLabel={dialog.mode === 'rename' ? 'Rename' : 'Move'}
          validate={(v) => validateMoveInput(dialog.mode, dialog.sourcePath, v)}
          onConfirm={onDialogConfirm}
          onCancel={() => setDialog(null)}
        />
      )}

      {collision && (
        <MoveCollisionWarning
          targetPath={collision.targetPath}
          busy={busy}
          onCancel={() => setCollision(null)}
          onOverwrite={() =>
            void doMove(
              collision.namespace,
              collision.project,
              collision.sourcePath,
              collision.targetPath,
              collision.currentEtag,
            )
          }
          onSaveAs={() => {
            const c = collision
            setCollision(null)
            // Reopen the move dialog prefilled with the attempted target so the
            // user picks a free path; a fresh collision reopens this warning
            // (bounded — each round carries its own target).
            setDialog({
              namespace: c.namespace,
              project: c.project,
              sourcePath: c.sourcePath,
              mode: 'move',
              prefill: c.targetPath,
            })
          }}
        />
      )}
    </Ctx.Provider>
  )
}

export function useMoveController(): MoveControllerApi {
  const ctx = useContext(Ctx)
  if (!ctx)
    throw new Error('useMoveController must be used within MoveProvider')
  return ctx
}
