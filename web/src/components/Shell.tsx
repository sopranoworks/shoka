import { useState, type ReactNode } from 'react'
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels'
import { DndProvider, useDrop } from 'react-dnd'
import { HTML5Backend } from 'react-dnd-html5-backend'
import { useRouterState } from '@tanstack/react-router'
import { TitleBar } from './TitleBar'
import { ActivityRail, type RailView } from './ActivityRail'
import { Sidebar } from './Sidebar'
import { SidebarTrash } from './SidebarTrash'
import { StatusBar } from './StatusBar'
import { CommandPalette } from './CommandPalette'
import { Banner } from './Banner'
import { Toaster } from './Toaster'
import { NotifyBridge } from './NotifyBridge'
import { MoveProvider } from '../lib/moveController'
import { TrashProvider, useTrashController } from '../lib/trashController'
import { useMediaQuery } from '../lib/useMediaQuery'
import {
  useRailSelect,
  useResetRailToExplorerOnProjectChange,
  useSettingsRailSync,
} from '../lib/useRailSelect'
import styles from './Shell.module.css'

/**
 * Persistent docked shell. Rendered once at the root route, it never unmounts.
 * Only `children` (the routed <Outlet/>) swaps on navigation.
 */
export function Shell({ children }: { children: ReactNode }) {
  const [rail, setRail] = useState<RailView>('explorer')
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const isNarrow = useMediaQuery('(max-width: 640px)')
  const { onSelect: onRailSelect, disabledItems } = useRailSelect(
    rail,
    sidebarOpen,
    setRail,
    setSidebarOpen,
  )
  // Selecting a project defaults the rail to Explorer (the file view).
  useResetRailToExplorerOnProjectChange(setRail)
  // A settings route (reload / deep-link) shows the Settings mode. Declared AFTER the
  // explorer-reset so it wins on a direct load of a project-scoped settings route.
  useSettingsRailSync(setRail)

  // On narrow screens the panel group stacks vertically so the content stays
  // full-width and readable (no sliver). On desktop it's a resizable split.
  const direction = isNarrow ? 'vertical' : 'horizontal'

  return (
    <MoveProvider>
    <TrashProvider>
    {/* ONE shared react-dnd manager (HTML5Backend) for the whole shell: the file
        tree (react-arborist, via FileTree's <Tree dndManager>) and the trash box
        (ShellRail's useDrop) participate in the SAME DnD context, so dragging a
        tree row onto the trash box is delivered by react-dnd natively — the same
        path as an in-tree move (B-31 RE-OPEN fix F). The earlier native-event
        bridge could not receive the drop: react-dnd suppresses the native drop over
        a non-react-dnd target. */}
    <DndProvider backend={HTML5Backend}>
    <div className={styles.shell} data-narrow={isNarrow}>
      <TitleBar onToggleSidebar={() => setSidebarOpen((v) => !v)} />

      <div className={styles.body}>
        <ShellRail
          active={rail}
          onSelect={onRailSelect}
          disabled={disabledItems}
        />

        <div className={styles.panelArea}>
          <PanelGroup
            key={direction}
            direction={direction}
            autoSaveId={isNarrow ? undefined : 'shoka-proto-layout'}
          >
            {sidebarOpen && (
              <>
                <Panel
                  id="sidebar"
                  order={1}
                  defaultSize={isNarrow ? 34 : 22}
                  minSize={isNarrow ? 18 : 14}
                  maxSize={isNarrow ? 60 : 40}
                  className={styles.sidebarPanel}
                >
                  {/* The sidebar column splits vertically: the view (file tree)
                      above, the trash pane as an in-column collapsible section
                      below (B-31 fix G). SidebarTrash renders nothing when the
                      pane is closed, so the view fills the column. */}
                  <Sidebar view={rail} />
                  <SidebarTrash />
                </Panel>
                <PanelResizeHandle
                  className={
                    isNarrow ? styles.resizeHandleH : styles.resizeHandle
                  }
                />
              </>
            )}
            <Panel id="content" order={2} className={styles.contentPanel}>
              <Banner />
              <main className={styles.content}>{children}</main>
            </Panel>
          </PanelGroup>
        </div>
      </div>

      <StatusBar />
      <CommandPalette />
      <Toaster />
      <NotifyBridge />
    </div>
    </DndProvider>
    </TrashProvider>
    </MoveProvider>
  )
}

// The dragged react-arborist item: its node id (a file path — see lib/tree
// toTreeNodes, id === path) plus the selected drag set. This is what react-dnd's
// type "NODE" carries (react-arborist drag-hook), so the trash drop reads the
// dropped file path(s) directly — no out-of-band drag-source bridge.
interface NodeDragItem {
  id: string
  dragIds: string[]
}

// The active project from the URL, so the trash drop knows which project's file was
// dropped (the tree only ever shows the active project).
function useActiveProjectRef(): { ns: string; proj: string } | null {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const m = pathname.match(/^\/p\/([^/]+)\/([^/]+)/)
  if (!m) return null
  return { ns: decodeURIComponent(m[1]), proj: decodeURIComponent(m[2]) }
}

// The activity rail wired to the trash controller and a react-dnd drop target. The
// trash box shows the queued count, opens/collapses the trash pane, AND is a
// first-class react-dnd drop target (accept "NODE"): when a tree row is dropped on
// it, react-dnd delivers the node and we enqueue that file through the SAME
// enqueuePath as right-click Delete… (deferred grace, etag unchanged). `isOver`
// drives the drop affordance. ShellRail lives inside Shell's DndProvider, so its
// useDrop shares the manager react-arborist uses (B-31 RE-OPEN fix F).
function ShellRail({
  active,
  onSelect,
  disabled,
}: {
  active: RailView
  onSelect: (v: RailView) => void
  disabled: RailView[]
}) {
  const { items, paneOpen, togglePane, enqueuePath } = useTrashController()
  const projectRef = useActiveProjectRef()
  const [{ isOver }, dropRef] = useDrop<NodeDragItem, void, { isOver: boolean }>(
    () => ({
      accept: 'NODE',
      canDrop: () => !!projectRef,
      drop: (item) => {
        if (!projectRef) return
        const paths = item.dragIds?.length ? item.dragIds : [item.id]
        for (const path of paths) {
          void enqueuePath({
            namespace: projectRef.ns,
            project: projectRef.proj,
            path,
          })
        }
      },
      collect: (m) => ({ isOver: m.isOver() && m.canDrop() }),
    }),
    [projectRef, enqueuePath],
  )
  return (
    <ActivityRail
      active={active}
      onSelect={onSelect}
      disabled={disabled}
      trashCount={items.length}
      trashActive={paneOpen}
      onTrashClick={togglePane}
      trashDropRef={dropRef}
      trashIsOver={isOver}
    />
  )
}
