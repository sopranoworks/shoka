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
}: {
  active: RailView
  onSelect: (v: RailView) => void
  // Rail items that are inert in the current context (e.g. Search/History on an
  // admin/no-project route, where they have no meaningful action). A disabled
  // item is genuinely inert: not clickable, no hover-active, dimmed, and
  // aria-disabled — never an active-looking button that does nothing.
  disabled?: RailView[]
}) {
  return (
    <nav className={styles.rail} aria-label="Activity bar">
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
  )
}
