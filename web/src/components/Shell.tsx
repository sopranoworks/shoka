import { useState, type ReactNode } from 'react'
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels'
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

  // On narrow screens the panel group stacks vertically so the content stays
  // full-width and readable (no sliver). On desktop it's a resizable split.
  const direction = isNarrow ? 'vertical' : 'horizontal'

  return (
    <MoveProvider>
    <TrashProvider>
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
    </TrashProvider>
    </MoveProvider>
  )
}

// The activity rail wired to the trash controller: the trash box shows the queued
// count, opens/collapses the trash pane, and is the drag-to-trash drop target. It
// lives below TrashProvider (a child of Shell's returned tree) so it can consume
// the context Shell mounts.
function ShellRail({
  active,
  onSelect,
  disabled,
}: {
  active: RailView
  onSelect: (v: RailView) => void
  disabled: RailView[]
}) {
  const {
    items,
    paneOpen,
    togglePane,
    enqueueFromDrag,
    onTrashDragEnter,
    onTrashDragLeave,
  } = useTrashController()
  return (
    <ActivityRail
      active={active}
      onSelect={onSelect}
      disabled={disabled}
      trashCount={items.length}
      trashActive={paneOpen}
      onTrashClick={togglePane}
      onTrashDrop={enqueueFromDrag}
      onTrashDragEnter={onTrashDragEnter}
      onTrashDragLeave={onTrashDragLeave}
    />
  )
}
