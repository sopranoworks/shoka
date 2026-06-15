import styles from './ActivityRail.module.css'

export type RailView = 'explorer' | 'search' | 'history'

interface Item {
  id: RailView
  label: string
  icon: React.ReactNode
}

const items: Item[] = [
  {
    id: 'explorer',
    label: 'Explorer',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <path
          d="M4 5.5h5l2 2h9v11H4z"
          stroke="currentColor"
          strokeWidth="1.6"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    id: 'search',
    label: 'Search',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <circle cx="10.5" cy="10.5" r="6" stroke="currentColor" strokeWidth="1.6" />
        <path d="M15 15l4.5 4.5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
      </svg>
    ),
  },
  {
    id: 'history',
    label: 'History',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <path d="M4 12a8 8 0 1 0 2.5-5.8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
        <path d="M4 4v3h3" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
        <path d="M12 8v4l3 2" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
    ),
  },
]

export function ActivityRail({
  active,
  onSelect,
  disabled = [],
  trashCount = 0,
  trashActive = false,
  onTrashClick,
  onTrashDrop,
}: {
  active: RailView
  onSelect: (v: RailView) => void
  // Rail items that are inert in the current context (e.g. Search/History on an
  // admin/no-project route, where they have no meaningful action). A disabled
  // item is genuinely inert: not clickable, no hover-active, dimmed, and
  // aria-disabled — never an active-looking button that does nothing.
  disabled?: RailView[]
  // The trash box at the bottom of the rail: it opens/collapses the trash pane
  // AND doubles as the drag-to-trash drop target (B-31). It is deliberately
  // SEPARATE from the Explorer/Search/History nav items (a distinct surface, its
  // own region), so the "exactly three activity items" invariant still holds.
  trashCount?: number
  trashActive?: boolean
  onTrashClick?: () => void
  onTrashDrop?: () => void
}) {
  return (
    <div className={styles.rail}>
      <nav className={styles.nav} aria-label="Activity bar">
        {items.map((it) => {
          const isDisabled = disabled.includes(it.id)
          return (
            <button
              key={it.id}
              className={styles.item}
              data-active={!isDisabled && active === it.id}
              data-disabled={isDisabled}
              disabled={isDisabled}
              aria-disabled={isDisabled}
              title={isDisabled ? `${it.label} — not available here` : it.label}
              aria-label={it.label}
              aria-pressed={!isDisabled && active === it.id}
              onClick={() => onSelect(it.id)}
            >
              {it.icon}
            </button>
          )
        })}
      </nav>

      <div className={styles.bottom}>
        <button
          type="button"
          className={styles.trash}
          data-active={trashActive}
          aria-label="Trash"
          aria-pressed={trashActive}
          title="Trash — files pending deletion (drop a file here to delete)"
          onClick={() => onTrashClick?.()}
          // Drop target for a dragged tree row (drag-to-trash). preventDefault on
          // dragover marks the box as a valid drop zone; the drop reads the file
          // recorded at drag-start (lib/dragSource) and reserves it.
          onDragOver={(e) => {
            e.preventDefault()
            if (e.dataTransfer) e.dataTransfer.dropEffect = 'move'
          }}
          onDrop={(e) => {
            e.preventDefault()
            onTrashDrop?.()
          }}
        >
          <TrashIcon />
          {trashCount > 0 && (
            <span className={styles.badge} aria-label={`${trashCount} queued`}>
              {trashCount}
            </span>
          )}
        </button>
      </div>
    </div>
  )
}

function TrashIcon() {
  return (
    <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
      <path
        d="M5 7h14M10 7V5h4v2M6 7l1 12h10l1-12"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}
