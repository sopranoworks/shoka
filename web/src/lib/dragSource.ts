// The file currently being dragged from the file tree, recorded at drag-start so
// a drop target OUTSIDE the react-arborist Tree (the activity-rail trash box) can
// learn which file was dropped on it. react-arborist owns its in-tree drag state
// (and its own dataTransfer payload), so an external drop zone cannot read it;
// this tiny module-level signal bridges the gap without coupling the rail to the
// tree. It is intentionally process-global and transient: set on dragstart,
// consumed on drop, and cleared on dragend.
//
// It carries no etag — the trash trigger captures the file's CURRENT etag with a
// fresh read at enqueue time (the if_match the deferred delete will use), so a
// stale drag payload can never carry a wrong etag.

export interface DragSource {
  namespace: string
  project: string
  path: string
}

let current: DragSource | null = null

// Whether the active drag is currently OVER the activity-rail trash box. Tracked
// from the rail's dragenter/dragleave (which fire regardless of dropEffect) so the
// source row's dragend — which ALWAYS fires, even when react-arborist's react-dnd
// HTML5Backend suppresses the rail's native `drop` over a non-dnd target — can tell
// whether the drag was released on the trash box and enqueue accordingly. This is
// the robust fallback that makes drag-to-trash work despite react-dnd owning the
// row drag (B-31 fix F).
let overTrash = false

export function setDragSource(src: DragSource | null): void {
  current = src
}

export function getDragSource(): DragSource | null {
  return current
}

// Clears the whole drag lifecycle: the dragged file AND the over-trash flag. Called
// when a drag ends (success or cancel) and after a drop is consumed, so the next
// drag starts clean and a consumed drag can never be re-enqueued.
export function clearDragSource(): void {
  current = null
  overTrash = false
}

export function setOverTrash(v: boolean): void {
  overTrash = v
}

export function isOverTrash(): boolean {
  return overTrash
}
