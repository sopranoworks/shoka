import type { ReactNode } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useDragDropManager } from 'react-dnd'
import {
  useToast,
  sidebarStyles,
  fileTreeStyles,
  type ContentConfig,
  type TreeNode,
} from '@shoka/web-core'
import { FileDropzone } from './components/FileDropzone'
import {
  useMoveController,
  dirOf,
  baseOf,
  joinPath,
  validateMoveInput,
} from './lib/moveController'
import { useTrashController } from './lib/trashController'

function ShokaNewFileButton({
  ns,
  proj,
  launchDir,
}: {
  ns: string
  proj: string
  launchDir: string
}) {
  const navigate = useNavigate()
  return (
    <button
      type="button"
      className={sidebarStyles.newFileBtn}
      title="New file"
      aria-label="New file"
      onClick={() =>
        void navigate({
          to: '/p/$namespace/$project/new',
          params: { namespace: ns, project: proj },
          search: launchDir ? { in: launchDir } : {},
        })
      }
    >
      + New file
    </button>
  )
}

function ShokaEditButton({
  ns,
  proj,
  path,
  className,
}: {
  ns: string
  proj: string
  path: string
  className?: string
}) {
  const navigate = useNavigate()
  return (
    <button
      className={className}
      title="Edit this file (⌘E)"
      onClick={() =>
        void navigate({
          to: '/p/$namespace/$project/edit/$',
          params: { namespace: ns, project: proj, _splat: path },
        })
      }
    >
      <svg width="13" height="13" viewBox="0 0 16 16" fill="none" aria-hidden="true">
        <path
          d="M11.5 2.5l2 2L6 12l-2.5.5L4 10z"
          stroke="currentColor"
          strokeWidth="1.2"
          strokeLinejoin="round"
        />
      </svg>
      Edit
    </button>
  )
}

function ShokaContextMenu({
  node,
  ns,
  proj,
  x,
  y,
  onClose,
  onRename,
  onCopyLink,
}: {
  node: TreeNode
  ns: string
  proj: string
  x: number
  y: number
  onClose: () => void
  onRename: () => void
  onCopyLink: () => void
}) {
  const { requestMove } = useMoveController()
  const { enqueuePath } = useTrashController()

  const item = (label: string, fn: () => void) => (
    <button
      className={fileTreeStyles.ctxItem}
      onClick={() => {
        onClose()
        fn()
      }}
    >
      {label}
    </button>
  )

  return (
    <div
      className={fileTreeStyles.ctxMenu}
      role="menu"
      aria-label="File actions"
      style={{ left: x, top: y }}
    >
      {item('Rename…', onRename)}
      {item('Move…', () =>
        requestMove({ namespace: ns, project: proj, sourcePath: node.path, mode: 'move' }),
      )}
      {item('Copy deep link', onCopyLink)}
      <button
        className={`${fileTreeStyles.ctxItem} ${fileTreeStyles.ctxItemDanger}`}
        onClick={() => {
          onClose()
          void enqueuePath({ namespace: ns, project: proj, path: node.path })
        }}
      >
        Delete…
      </button>
    </div>
  )
}

export function useShokaContentConfig(): ContentConfig {
  const { executeMove } = useMoveController()
  const { items: trashItems, enqueuePath: _enqueuePath } = useTrashController()
  const { add: addToast } = useToast()

  const dndMgr = useDragDropManager()

  const reserved = new Set<string>()
  for (const it of trashItems) reserved.add(it.path)

  return {
    renderEditButton: (ns, proj, path, pageStyles) => (
      <ShokaEditButton ns={ns} proj={proj} path={path} className={pageStyles.editBtn} />
    ),
    renderNewFileLink: (ns, proj, pageStyles) => (
      <Link
        to="/p/$namespace/$project/new"
        params={{ namespace: ns, project: proj }}
        className={pageStyles.ghostBtn}
      >
        New file
      </Link>
    ),
    renderNewFileButton: (ns, proj, launchDir) => (
      <ShokaNewFileButton ns={ns} proj={proj} launchDir={launchDir} />
    ),
    renderDropZone: (ns, proj, children) => (
      <FileDropzone namespace={ns} project={proj}>
        {children}
      </FileDropzone>
    ),
    renderContextMenu: ({ node, ns, proj, x, y, onClose, onRename, onCopyLink }) => (
      <ShokaContextMenu
        node={node}
        ns={ns}
        proj={proj}
        x={x}
        y={y}
        onClose={onClose}
        onRename={onRename}
        onCopyLink={onCopyLink}
      />
    ),
    canDrag: true,
    canRename: true,
    onTreeMove: (ns, proj, src, dest) => {
      void executeMove({ namespace: ns, project: proj, sourcePath: src, targetPath: dest })
    },
    onTreeRename: (ns, proj, src, newName) => {
      const err = validateMoveInput('rename', src, newName)
      if (err) {
        if (newName.trim() && newName.trim() !== baseOf(src))
          addToast({ level: 'warn', text: err })
        return
      }
      void executeMove({
        namespace: ns,
        project: proj,
        sourcePath: src,
        targetPath: joinPath(dirOf(src), newName),
      })
    },
    reservedPaths: reserved,
    dndManager: dndMgr,
  }
}
