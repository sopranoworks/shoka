package storage

// Per-project derivative sibling DBs. A project directory <base>/<ns>/<proj> has up to
// four derivative sibling files alongside it: the catalog <proj>.db, the index
// <proj>.index.db, the deleted-log <proj>.deleted.db, and the vector index
// <proj>.vector.db (the last added 2026-07-05). They
// are disposable derivatives (rebuildable from git), but every lifecycle op that touches a
// project — delete, move, rename, leftover relocation, orphan clean — MUST account for ALL
// of them, or it strands a sibling (a stray DB with no project) that the health check then
// flags as orphaned. Before this helper existed each op hard-coded catalog+index and the
// deleted-log (the newest sibling) was forgotten by DeleteProject/move/rename/leftover/clean
// — the bug class behind the namespace-orphaned mis-detection. This is the SINGLE source of
// truth for "what files travel with a project", so adding a future sibling is one edit here.

// siblingDBPaths returns the absolute paths of every per-project derivative sibling DB for
// (namespace, projectName), in a stable order (catalog, index, deleted-log, vector). The
// files need not exist; callers stat/remove/relocate best-effort. Use this everywhere a
// project's sibling DBs are removed or relocated so no op forgets one.
func (s *FSGitStorage) siblingDBPaths(namespace, projectName string) []string {
	return []string{
		s.catalogPath(namespace, projectName),
		s.indexPath(namespace, projectName),
		s.deletedLogPath(namespace, projectName),
		s.vectorPath(namespace, projectName),
	}
}
