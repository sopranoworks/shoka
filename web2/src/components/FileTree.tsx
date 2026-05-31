import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { Tree, type NodeRendererProps, type TreeApi } from 'react-arborist'
import { buildTree } from '../lib/data'
import type { MockFile, TreeNode } from '../lib/types'
import styles from './FileTree.module.css'

/**
 * react-arborist file tree.
 * - arrow keys navigate, Enter / click both open a file (onActivate)
 * - directories expand/collapse on toggle
 * - the active file (from the URL) is highlighted and selected
 */
export function FileTree({
  namespace,
  project,
  files,
  activePath,
}: {
  namespace: string
  project: string
  files: MockFile[]
  activePath: string | null
}) {
  const navigate = useNavigate()
  const data = useMemo(() => buildTree(files), [files])
  const treeRef = useRef<TreeApi<TreeNode> | null>(null)

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
      <span className={styles.icon}>{node.data.isFile ? <FileIcon /> : <DirIcon open={node.isOpen} />}</span>
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
        d={
          open
            ? 'M2 4.5h4l1.2 1.4H14V13H2z'
            : 'M2 4.5h4l1.2 1.4H14V13H2z'
        }
        stroke="currentColor"
        strokeWidth="1.2"
        strokeLinejoin="round"
        fill={open ? 'currentColor' : 'none'}
        fillOpacity={open ? 0.12 : 0}
      />
    </svg>
  )
}
