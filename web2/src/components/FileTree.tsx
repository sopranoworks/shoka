import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Tree, type NodeRendererProps, type TreeApi } from 'react-arborist'
import { toTreeNodes, ancestorDirs } from '../lib/tree'
import type { FileNode, TreeNode } from '../lib/types'
import styles from './FileTree.module.css'

/**
 * react-arborist file tree, fed the backend's GET_TREE FileNode list.
 * - arrow keys navigate, Enter / click both open a file (onActivate)
 * - directories expand/collapse on toggle
 * - the active file (from the URL) is highlighted, selected, scrolled into
 *   view, and its ancestor directories are expanded (expand-to-active)
 */
export function FileTree({
  namespace,
  project,
  nodes,
  activePath,
}: {
  namespace: string
  project: string
  nodes: FileNode[]
  activePath: string | null
}) {
  const navigate = useNavigate()
  const data = useMemo(() => toTreeNodes(nodes), [nodes])
  const treeRef = useRef<TreeApi<TreeNode> | null>(null)

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
      to: '/p/$namespace/$project/blob/$',
      params: { namespace, project, _splat: path },
    })
  }

  return (
    <div ref={wrapRef} className={styles.wrap}>
      <Tree<TreeNode>
        ref={treeRef}
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
        onActivate={(node) => {
          if (node.data.isFile) openFile(node.data.path)
          else node.toggle()
        }}
      >
        {(props) => <Row {...props} activePath={activePath} />}
      </Tree>
    </div>
  )
}

function Row({
  node,
  style,
  dragHandle,
  activePath,
}: NodeRendererProps<TreeNode> & { activePath: string | null }) {
  const isActive = node.data.isFile && node.data.path === activePath
  return (
    <div
      ref={dragHandle}
      style={style}
      className={styles.row}
      data-active={isActive}
      onClick={(e) => {
        e.stopPropagation()
        if (node.data.isFile) {
          // activation handled by onActivate; also focus the row
          node.activate()
        } else {
          node.toggle()
        }
      }}
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
      <span className={styles.name}>{node.data.name}</span>
    </div>
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
