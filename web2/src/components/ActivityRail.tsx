import styles from './ActivityRail.module.css'

export type RailView = 'explorer' | 'search' | 'history' | 'namespaces'

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
  {
    id: 'namespaces',
    label: 'Namespaces',
    icon: (
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none">
        <rect x="4" y="4" width="7" height="7" rx="1.4" stroke="currentColor" strokeWidth="1.6" />
        <rect x="13" y="4" width="7" height="7" rx="1.4" stroke="currentColor" strokeWidth="1.6" />
        <rect x="4" y="13" width="7" height="7" rx="1.4" stroke="currentColor" strokeWidth="1.6" />
        <rect x="13" y="13" width="7" height="7" rx="1.4" stroke="currentColor" strokeWidth="1.6" />
      </svg>
    ),
  },
]

export function ActivityRail({
  active,
  onSelect,
}: {
  active: RailView
  onSelect: (v: RailView) => void
}) {
  return (
    <nav className={styles.rail} aria-label="Activity bar">
      {items.map((it) => (
        <button
          key={it.id}
          className={styles.item}
          data-active={active === it.id}
          title={it.label}
          aria-label={it.label}
          aria-pressed={active === it.id}
          onClick={() => onSelect(it.id)}
        >
          {it.icon}
        </button>
      ))}
    </nav>
  )
}
