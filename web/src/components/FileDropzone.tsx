import { useCallback, type ReactNode } from 'react'
import { useDrop } from 'react-dnd'
import { NativeTypes } from 'react-dnd-html5-backend'
import { useQueryClient } from '@tanstack/react-query'
import { useToast } from '../lib/toast'
import { addDroppedFile, type AddOutcome } from '../lib/fileAdd'
import styles from './FileDropzone.module.css'

// External file drag-and-drop ADD (B-28). A NATIVE file dropzone over the explorer
// panel: it accepts OS files (react-dnd NativeTypes.FILE / DataTransfer.files), a
// DIFFERENT drop type from the internal-node 'NODE' trash drag — so dropping a
// tree row is still a trash/move, and dropping an OS file ADDS it. The actual
// ingest reuses the existing /ws/ui SAVE_FILE base64 path (lib/fileAdd).

// What HTML5Backend delivers for a native OS-file drop: the dropped File list
// (populated at drop time, empty during hover).
interface NativeFileDropItem {
  files: File[]
}

// resolveDestDir maps the drop point to a destination folder: the folder row under
// the pointer (its data-dir-path), else the project root (''). Reading it from the
// DOM lets a single panel-level dropzone support folder-targeting without making
// every virtualized tree row its own drop target. The drag overlay sets
// pointer-events:none so it never shadows the row here.
function resolveDestDir(offset: { x: number; y: number } | null): string {
  if (!offset) return ''
  const el = document.elementFromPoint(offset.x, offset.y)
  const dir = el?.closest('[data-dir-path]') as HTMLElement | null
  return dir?.dataset.dirPath ?? ''
}

export function FileDropzone({
  namespace,
  project,
  children,
}: {
  namespace: string
  project: string
  children: ReactNode
}) {
  const qc = useQueryClient()
  const { add: addToast } = useToast()

  const onDrop = useCallback(
    async (files: File[], destDir: string) => {
      if (!files.length) return
      const confirmOverwrite = (path: string) =>
        window.confirm(`A file already exists at ${path}. Overwrite it?`)

      const outcomes: AddOutcome[] = []
      for (const file of files) {
        // Sequential: a per-file confirm dialog must not race, and the count is
        // small (a hand drop). Each file is independent — one rejection does not
        // stop the rest.
        outcomes.push(
          await addDroppedFile({ namespace, project, destDir, file, confirmOverwrite }),
        )
      }

      // Anything that landed → refresh the tree (the existing GET_TREE query).
      const landed = outcomes.filter(
        (o) => o.status === 'added' || o.status === 'overwritten',
      )
      if (landed.length) {
        void qc.invalidateQueries({ queryKey: ['tree', namespace, project] })
      }

      // Per-file feedback. 'warn' is the only toast level; rejections, errors and
      // skips are surfaced individually so the operator sees exactly what was NOT
      // added and why; successes get one concise summary (the tree refresh is the
      // primary success signal).
      for (const o of outcomes) {
        if (o.status === 'rejected' || o.status === 'error' || o.status === 'skipped') {
          addToast({ level: 'warn', text: o.message ?? `${o.name}: ${o.status}` })
        }
      }
      if (landed.length === 1) {
        addToast({ level: 'warn', text: `Added ${landed[0].path}` })
      } else if (landed.length > 1) {
        addToast({ level: 'warn', text: `Added ${landed.length} files` })
      }
    },
    [namespace, project, qc, addToast],
  )

  const [{ isOver }, dropRef] = useDrop<NativeFileDropItem, void, { isOver: boolean }>(
    () => ({
      accept: [NativeTypes.FILE],
      drop: (item, monitor) => {
        if (monitor.didDrop()) return // a nested target already handled it
        const destDir = resolveDestDir(monitor.getClientOffset())
        void onDrop(item.files ?? [], destDir)
      },
      collect: (m) => ({ isOver: m.isOver({ shallow: true }) && m.canDrop() }),
    }),
    [onDrop],
  )

  return (
    <div
      // react-dnd's drop connector returns a value; wrap it so the ref callback is
      // void (the connector attaches the DOM node as a side effect).
      ref={(node) => {
        dropRef(node)
      }}
      className={styles.zone}
      data-drop-active={isOver}
      data-testid="file-dropzone"
    >
      {children}
      {isOver && (
        <div className={styles.overlay} aria-hidden="true">
          <span className={styles.hint}>Drop to add files</span>
        </div>
      )}
    </div>
  )
}
