import { useState, type ReactNode } from 'react'
import { Panel, PanelGroup, PanelResizeHandle } from 'react-resizable-panels'
import { TitleBar } from './TitleBar'
import { ActivityRail, type RailView } from './ActivityRail'
import { Sidebar } from './Sidebar'
import { StatusBar } from './StatusBar'
import { CommandPalette } from './CommandPalette'
import { Banner } from './Banner'
import { Toaster } from './Toaster'
import { NotifyBridge } from './NotifyBridge'
import { useMediaQuery } from '../lib/useMediaQuery'
import styles from './Shell.module.css'

/**
 * Persistent docked shell. Rendered once at the root route, it never unmounts.
 * Only `children` (the routed <Outlet/>) swaps on navigation.
 */
export function Shell({ children }: { children: ReactNode }) {
  const [rail, setRail] = useState<RailView>('explorer')
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const isNarrow = useMediaQuery('(max-width: 640px)')

  // On narrow screens the panel group stacks vertically so the content stays
  // full-width and readable (no sliver). On desktop it's a resizable split.
  const direction = isNarrow ? 'vertical' : 'horizontal'

  return (
    <div className={styles.shell} data-narrow={isNarrow}>
      <TitleBar onToggleSidebar={() => setSidebarOpen((v) => !v)} />

      <div className={styles.body}>
        <ActivityRail
          active={rail}
          onSelect={(v) => {
            setRail(v)
            setSidebarOpen(true)
          }}
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
                  <Sidebar view={rail} />
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
  )
}
