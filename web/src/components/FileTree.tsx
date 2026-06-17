import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import {
  Tree,
  type NodeApi,
  type NodeRendererProps,
  type TreeApi,
} from 'react-arborist'
import { useDragDropManager } from 'react-dnd'
import { toTreeNodes, ancestorDirs } from '../lib/tree'
import {
  useMoveController,
  dirOf,
  baseOf,
  joinPath,
  validateMoveInput,
} from '../lib/moveController'
import { useToast } from '../lib/toast'
import { useTrashController } from '../lib/trashController'
import type { FileNode, TreeNode } from '../lib/types'
import styles from './FileTree.module.css'

/**
 * react-arborist file tree, fed the backend's GET_TREE FileNode list.
 * - arrow keys navigate, Enter / click both open a file (onActivate)
 * - directories expand/collapse on toggle
 * - the active file (from the URL) is highlighted, selected, scrolled into
 *   view, and its ancestor directories are expanded (expand-to-active)
 * - a file leaf can be renamed inline (commit -> moveFile same-dir, new name),
 *   right-clicked for a context menu (Rename… / Move… / Copy deep link), and
 *   dragged onto a directory to move it. All mutations funnel through the move
 *   controller (lib/moveController), never wsClient directly. A move is a pure
 *   path change — no link surface anywhere.
 */
// Which route a tree file opens into: the file view (default) or — in the
// sidebar's History mode — that file's history. Exported for a focused unit test.
export type TreeOpenMode = 'blob' | 'history'
export function fileOpenRoute(openMode: TreeOpenMode) {
  return openMode === 'history'
    ? '/p/$namespace/$project/history/$'
    : '/p/$namespace/$project/blob/$'
}

export function FileTree({
  namespace,
  project,
  nodes,
  activePath,
  openMode = 'blob',
}: {
  namespace: string
  project: string
  nodes: FileNode[]
  activePath: string | null
  // In History mode the tree opens each file's history (the right pane follows
  // the selected file) instead of the file view. Default: open the file.
  openMode?: TreeOpenMode
}) {
  const navigate = useNavigate()
  const { requestMove, executeMove } = useMoveController()
  const { enqueuePath, items: trashItems } = useTrashController()
  const { add: addToast } = useToast()
  // The shared react-dnd manager from Shell's DndProvider: passing it to <Tree
  // dndManager> makes react-arborist use the SAME manager the trash box's useDrop
  // uses, so a row dragged onto the trash box is delivered by react-dnd (B-31
  // RE-OPEN fix F). Without this, arborist would spin up its own HTML5 backend and
  // the trash box could never receive an arborist drag.
  const dndManager = useDragDropManager()
  const data = useMemo(() => toTreeNodes(nodes), [nodes])
  const treeRef = useRef<TreeApi<TreeNode> | null>(null)

  // Paths reserved in the trash (this project): their rows render pending
  // (dimmed/struck) so a queued file reads as "on its way out" during the grace.
  const reserved = useMemo(() => {
    const s = new Set<string>()
    for (const it of trashItems)
      if (it.namespace === namespace && it.project === project) s.add(it.path)
    return s
  }, [trashItems, namespace, project])

  // Right-click context menu state: anchor position + the file it targets.
  const [menu, setMenu] = useState<{ x: number; y: number; node: TreeNode } | null>(
    null,
  )

  // Expand ancestors of the active file on first paint.
  const initialOpenState = useMemo(() => {
    const open: Record<string, boolean> = {}
    if (activePath) for (const dir of ancestorDirs(activePath)) open[dir] = true
    return open
  }, [activePath])

  // Measure the container so the virtualized tree fills the sidebar.
  const wrapRef = useRef<HTMLDivElement>(null)
  const [size, setSize] = useState({ w: 240, h: 400 })
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    const ro = new ResizeObserver(() => {
      setSize({ w: el.clientWidth, h: el.clientHeight })
    })
    ro.observe(el)
    setSize({ w: el.clientWidth, h: el.clientHeight })
    return () => ro.disconnect()
  }, [])

  // Expand-to-active and scroll into view when the active file (or tree) changes
  // after mount — e.g. navigating to a deep file via quick-open or a deep link.
  useEffect(() => {
    if (!activePath) return
    const api = treeRef.current
    if (!api) return
    for (const dir of ancestorDirs(activePath)) api.open(dir)
    api.scrollTo(activePath)
  }, [activePath, data])

  const openFile = (path: string) => {
    navigate({
      to: fileOpenRoute(openMode),
      params: { namespace, project, _splat: path },
    })
  }

  // Inline-rename commit: react-arborist hands us the new basename; resolve it
  // against the source directory and move there. Reject a malformed name (slash,
  // ./.. etc.) with a toast so the user gets feedback (the tree reverts on its
  // own since no move is issued).
  const onRename = ({ name, node }: { name: string; node: NodeApi<TreeNode> }) => {
    if (!node.data.isFile) return
    const src = node.data.path
    const err = validateMoveInput('rename', src, name)
    if (err) {
      if (name.trim() && name.trim() !== baseOf(src))
        addToast({ level: 'warn', text: err })
      return
    }
    void executeMove({
      namespace,
      project,
      sourcePath: src,
      targetPath: joinPath(dirOf(src), name.trim()),
    })
  }

  // Drag-and-drop: dropping a file leaf onto a directory (or root) moves it
  // there, keeping its basename. Folder drags are disabled (disableDrag); a
  // same-directory drop is a no-op (executeMove guards same-path).
  const onMove = ({
    dragNodes,
    parentNode,
  }: {
    dragNodes: NodeApi<TreeNode>[]
    parentNode: NodeApi<TreeNode> | null
  }) => {
    const dragged = dragNodes[0]
    if (!dragged || !dragged.data.isFile) return
    const src = dragged.data.path
    // parentNode null => dropped at the project root; else the target directory.
    const destDir = parentNode ? parentNode.data.path : ''
    const target = joinPath(destDir, baseOf(src))
    void executeMove({ namespace, project, sourcePath: src, targetPath: target })
  }

  const copyDeepLink = (path: string) => {
    const url = `${window.location.origin}/p/${encodeURIComponent(
      namespace,
    )}/${encodeURIComponent(project)}/blob/${path}`
    void navigator.clipboard?.writeText(url).catch(() => {})
  }

  return (
    <div ref={wrapRef} className={styles.wrap}>
      <Tree<TreeNode>
        ref={treeRef}
        dndManager={dndManager}
        data={data}
        idAccessor="id"
        childrenAccessor="children"
        initialOpenState={initialOpenState}
        openByDefault={false}
        width={size.w}
        height={size.h}
        indent={14}
        rowHeight={24}
        disableMultiSelection
        selection={activePath ?? undefined}
        onMove={onMove}
        onRename={onRename}
        // disableEdit/disableDrag receive the node DATA (BoolFunc<T>): only file
        // leaves can be renamed inline or dragged (folder move is out of scope).
        disableEdit={(data: TreeNode) => !data.isFile}
        disableDrag={(data: TreeNode) => !data.isFile}
        onActivate={(node) => {
          if (node.data.isFile) openFile(node.data.path)
          else node.toggle()
        }}
      >
        {(props) => (
          <Row
            {...props}
            activePath={activePath}
            isReserved={props.node.data.isFile && reserved.has(props.node.data.path)}
            onContext={(e, node) => {
              if (!node.isFile) return
              e.preventDefault()
              setMenu({ x: e.clientX, y: e.clientY, node })
            }}
          />
        )}
      </Tree>

      {menu && (
        <RowContextMenu
          x={menu.x}
          y={menu.y}
          onClose={() => setMenu(null)}
          onRename={() => {
            void treeRef.current?.edit(menu.node.id)
          }}
          onMove={() =>
            requestMove({
              namespace,
              project,
              sourcePath: menu.node.path,
              mode: 'move',
            })
          }
          onCopyLink={() => copyDeepLink(menu.node.path)}
          onDelete={() =>
            void enqueuePath({ namespace, project, path: menu.node.path })
          }
        />
      )}
    </div>
  )
}

function RowContextMenu({
  x,
  y,
  onClose,
  onRename,
  onMove,
  onCopyLink,
  onDelete,
}: {
  x: number
  y: number
  onClose: () => void
  onRename: () => void
  onMove: () => void
  onCopyLink: () => void
  onDelete: () => void
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const item = (label: string, fn: () => void) => (
    <button
      className={styles.ctxItem}
      onClick={() => {
        onClose()
        fn()
      }}
    >
      {label}
    </button>
  )

  return (
    <>
      <div className={styles.ctxOverlay} onClick={onClose} onContextMenu={(e) => {
        e.preventDefault()
        onClose()
      }} />
      <div
        className={styles.ctxMenu}
        style={{ left: x, top: y }}
        role="menu"
        aria-label="File actions"
      >
        {item('Rename…', onRename)}
        {item('Move…', onMove)}
        {item('Copy deep link', onCopyLink)}
        {/* Delete… enqueues the file into the trash (deferred-execution grace);
            it does NOT delete on click — Cancel in the trash pane reverses it
            with no write, no conflict. */}
        <button
          className={`${styles.ctxItem} ${styles.ctxItemDanger}`}
          onClick={() => {
            onClose()
            onDelete()
          }}
        >
          Delete…
        </button>
      </div>
    </>
  )
}

function Row({
  node,
  style,
  dragHandle,
  activePath,
  isReserved,
  onContext,
}: NodeRendererProps<TreeNode> & {
  activePath: string | null
  isReserved: boolean
  onContext: (e: React.MouseEvent, node: TreeNode) => void
}) {
  const isActive = node.data.isFile && node.data.path === activePath
  return (
    <div
      ref={dragHandle}
      style={style}
      className={styles.row}
      data-active={isActive}
      data-reserved={isReserved}
      // Folder rows carry their path so the external-file dropzone (FileDropzone)
      // can resolve a drop onto this folder to that destination prefix (B-28); file
      // rows have none, so a drop on a file falls through to the project root.
      data-dir-path={!node.data.isFile ? node.data.path : undefined}
      // The drag is driven entirely by react-arborist's react-dnd useDrag (via the
      // dragHandle ref). Dropping a row on the trash box is delivered by react-dnd to
      // ShellRail's useDrop, which reads the node id (a file path) — no out-of-band
      // drag bridge (B-31 RE-OPEN fix F).
      onClick={(e) => {
        e.stopPropagation()
        if (node.data.isFile) {
          // activation handled by onActivate; also focus the row
          node.activate()
        } else {
          node.toggle()
        }
      }}
      onContextMenu={(e) => onContext(e, node.data)}
    >
      <span className={styles.chevron} data-dir={!node.data.isFile}>
        {!node.data.isFile && (
          <svg
            width="12"
            height="12"
            viewBox="0 0 12 12"
            style={{
              transform: node.isOpen ? 'rotate(90deg)' : 'none',
              transition: 'transform 0.1s',
            }}
          >
            <path d="M4 2.5L8 6l-4 3.5z" fill="currentColor" />
          </svg>
        )}
      </span>
      <span className={styles.icon}>
        {node.data.isFile ? <FileIcon /> : <DirIcon open={node.isOpen} />}
      </span>
      {node.isEditing ? (
        <RenameInput node={node} />
      ) : (
        <span className={styles.name}>{node.data.name}</span>
      )}
    </div>
  )
}

// The inline-rename field. Mirrors react-arborist's default edit form: focus +
// select on mount, Enter submits (fires onRename), Escape / blur cancels.
function RenameInput({ node }: { node: NodeApi<TreeNode> }) {
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => {
    ref.current?.focus()
    ref.current?.select()
  }, [])
  return (
    <input
      ref={ref}
      className={styles.editInput}
      defaultValue={node.data.name}
      onClick={(e) => e.stopPropagation()}
      onBlur={() => node.reset()}
      onKeyDown={(e) => {
        e.stopPropagation()
        if (e.key === 'Escape') node.reset()
        if (e.key === 'Enter') node.submit(ref.current?.value ?? '')
      }}
    />
  )
}

function FileIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" fill="none">
      <path
        d="M4 2h5l3 3v9H4z"
        stroke="currentColor"
        strokeWidth="1.2"
        strokeLinejoin="round"
      />
      <path d="M9 2v3h3" stroke="currentColor" strokeWidth="1.2" strokeLinejoin="round" />
    </svg>
  )
}

function DirIcon({ open }: { open: boolean }) {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" fill="none">
      <path
        d="M2 4.5h4l1.2 1.4H14V13H2z"
        stroke="currentColor"
        strokeWidth="1.2"
        strokeLinejoin="round"
        fill={open ? 'currentColor' : 'none'}
        fillOpacity={open ? 0.12 : 0}
      />
    </svg>
  )
}
