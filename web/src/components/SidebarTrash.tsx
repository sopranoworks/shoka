import { useTrashController } from '../lib/trashController'
import { TrashPane } from './TrashPane'

// SidebarTrash mounts the trash pane as an in-column collapsible section at the
// bottom of the sidebar column (B-31 fix G): it renders only while the pane is open,
// splitting the column vertically (the file tree yields space above) instead of
// floating over the sidebar as a dialog/overflow popover. It consumes the same
// TrashProvider context as the activity-rail trash box, so the rail's open/collapse
// toggle and the auto-open/auto-collapse rule drive it directly.
export function SidebarTrash() {
  const { items, paneOpen, cancel, executeNow, closePane } = useTrashController()
  if (!paneOpen) return null
  return (
    <TrashPane
      items={items}
      onCancel={cancel}
      onDeleteNow={executeNow}
      onClose={closePane}
    />
  )
}
