import { useCallback, useState, type ReactNode } from 'react'
import { useDrop } from 'react-dnd'
import { NativeTypes } from 'react-dnd-html5-backend'
import { useQueryClient } from '@tanstack/react-query'
import {
  useToast,
  ConversionConfirmDialog,
  prepareConversions,
  convertCandidate,
  needsConversion,
  type ConversionCandidate,
} from '@shoka/web-core'
import { addDroppedFile, type AddOutcome } from '../lib/fileAdd'
import styles from './FileDropzone.module.css'

interface NativeFileDropItem {
  files: File[]
}

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

  const [conversionState, setConversionState] = useState<{
    candidates: ConversionCandidate[]
    resolve: (confirmed: boolean) => void
  } | null>(null)

  const onDrop = useCallback(
    async (files: File[], destDir: string) => {
      if (!files.length) return
      const confirmOverwrite = (path: string) =>
        window.confirm(`A file already exists at ${path}. Overwrite it?`)

      const convertible = files.filter((f) => needsConversion(f.name))
      const passThrough = files.filter((f) => !needsConversion(f.name))

      let convertedFiles: File[] = []
      if (convertible.length > 0) {
        const { candidates, errors } = await prepareConversions(convertible)

        for (const err of errors) {
          addToast({ level: 'warn', text: err })
        }

        if (candidates.length > 0) {
          const confirmed = await new Promise<boolean>((resolve) => {
            setConversionState({ candidates, resolve })
          })
          setConversionState(null)

          if (confirmed) {
            convertedFiles = candidates.map(convertCandidate)
          }
        }
      }

      const allFiles = [...passThrough, ...convertedFiles]
      const outcomes: AddOutcome[] = []
      for (const file of allFiles) {
        outcomes.push(
          await addDroppedFile({ namespace, project, destDir, file, confirmOverwrite }),
        )
      }

      const landed = outcomes.filter(
        (o) => o.status === 'added' || o.status === 'overwritten',
      )
      if (landed.length) {
        void qc.invalidateQueries({ queryKey: ['tree', namespace, project] })
      }

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
        if (monitor.didDrop()) return
        const destDir = resolveDestDir(monitor.getClientOffset())
        void onDrop(item.files ?? [], destDir)
      },
      collect: (m) => ({ isOver: m.isOver({ shallow: true }) && m.canDrop() }),
    }),
    [onDrop],
  )

  return (
    <div
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
      <ConversionConfirmDialog
        open={conversionState !== null}
        candidates={conversionState?.candidates ?? []}
        onConfirm={() => conversionState?.resolve(true)}
        onCancel={() => conversionState?.resolve(false)}
      />
    </div>
  )
}
