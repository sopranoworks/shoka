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

export function setDragSource(src: DragSource | null): void {
  current = src
}

export function getDragSource(): DragSource | null {
  return current
}

export function clearDragSource(): void {
  current = null
}
