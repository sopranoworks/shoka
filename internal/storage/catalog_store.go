package storage

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/catalog"
)

// catalogPath returns the on-disk path of a project's catalog DB:
// <base_dir>/<namespace>/@<project>.project.db (alongside the project directory,
// not inside it — see the catalog design log §5). The leading @ distinguishes
// sibling DB files from project directories.
func (s *FSGitStorage) catalogPath(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, "@"+projectName+".project.db")
}

// migrateLegacySiblings renames pre-@ sibling DB files to the @-prefixed pattern
// at startup. Handles all four kinds and both legacy naming schemes:
//   - v0 catalog: <project>.db → @<project>.project.db
//   - v1 catalog: <project>.project.db → @<project>.project.db
//   - index/deleted/vector: <project>.<kind>.db → @<project>.<kind>.db
func (s *FSGitStorage) migrateLegacySiblings(namespace, projectName string) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	nsDir := filepath.Join(s.baseDir, namespace)
	renameLegacy := func(old, target string) {
		if _, err := os.Stat(old); err != nil {
			return
		}
		if _, err := os.Stat(target); err == nil {
			return
		}
		if err := os.Rename(old, target); err != nil {
			s.log().Warn("failed to migrate legacy sibling DB",
				"namespace", namespace, "project", projectName,
				"from", filepath.Base(old), "to", filepath.Base(target), "err", err)
		}
	}
	// v0 catalog: <project>.db (no kind, no @)
	renameLegacy(
		filepath.Join(nsDir, projectName+".db"),
		s.catalogPath(namespace, projectName),
	)
	// v1/pre-@ siblings: <project>.<kind>.db (no @)
	for _, kind := range []string{"project", "index", "deleted", "vector"} {
		renameLegacy(
			filepath.Join(nsDir, projectName+"."+kind+".db"),
			filepath.Join(nsDir, "@"+projectName+"."+kind+".db"),
		)
	}
}

// registerCatalog records an already-open catalog handle. Used by the startup
// gate and rebuild. Any handle previously registered for the key is closed.
func (s *FSGitStorage) registerCatalog(namespace, projectName string, c *catalog.Catalog) {
	key := projectKey(namespace, projectName)
	s.catMu.Lock()
	if old, ok := s.catalogs[key]; ok && old != nil && old != c {
		_ = old.Close()
	}
	s.catalogs[key] = c
	s.catMu.Unlock()
}

// catalogFor returns the open catalog for a project, opening it (or creating an
// empty one if the DB file does not yet exist) on demand. It is the mutating
// path's accessor: write_file, delete_file, and create_project rely on a
// catalog existing. A corrupt or schema-mismatched DB is an error here (the
// startup gate is responsible for rebuilds from git); callers on the mutation
// path treat the error as best-effort and never fail the operation on it.
func (s *FSGitStorage) catalogFor(namespace, projectName string) (*catalog.Catalog, error) {
	key := projectKey(namespace, projectName)
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if c, ok := s.catalogs[key]; ok && c != nil {
		return c, nil
	}
	p := s.catalogPath(namespace, projectName)
	c, err := catalog.Open(p)
	if errors.Is(err, catalog.ErrNotFound) {
		c, err = catalog.Create(p, namespace, projectName)
	}
	if err != nil {
		return nil, err
	}
	s.catalogs[key] = c
	return c, nil
}

// catalogForRead returns the open catalog for a project without ever creating
// one. Used by the read path's ENOENT invariant check so that a read never
// writes an @<project>.project.db file. Returns nil if no catalog is registered and none can be
// opened.
func (s *FSGitStorage) catalogForRead(namespace, projectName string) *catalog.Catalog {
	key := projectKey(namespace, projectName)
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if c, ok := s.catalogs[key]; ok && c != nil {
		return c
	}
	c, err := catalog.Open(s.catalogPath(namespace, projectName))
	if err != nil {
		return nil
	}
	s.catalogs[key] = c
	return c
}

// catalogPut records a successful write in the catalog. Best-effort: a failure
// is logged and counted but never fails the write (the working tree and WAL
// have already succeeded; the next rebuild reconciles). modified_at is the
// working-tree file's mtime so it stays byte-identical with read_summary and
// list_files (the 2026-05-30 modified-at contract).
func (s *FSGitStorage) catalogPut(namespace, projectName, rel, etag string, size int, fullPath string) {
	c, err := s.catalogFor(namespace, projectName)
	if err != nil {
		s.log().Warn("catalog unavailable for write_file",
			"namespace", namespace, "project", projectName, "path", rel, "err", err)
		s.catUpdateFailedWrite.Add(1)
		return
	}
	mt := time.Now().UTC()
	if info, serr := os.Stat(fullPath); serr == nil {
		mt = info.ModTime().UTC()
	}
	if perr := c.PutFile(rel, catalog.FileEntry{Etag: etag, Size: int64(size), ModifiedAt: mt}); perr != nil {
		s.log().Warn("catalog update failed for write_file",
			"namespace", namespace, "project", projectName, "path", rel, "err", perr)
		s.catUpdateFailedWrite.Add(1)
	}
}

// catalogDelete removes a path from the catalog before the working-tree removal
// (delete ordering, design log §6.2). Best-effort: a failure is logged and
// counted but never fails the delete.
func (s *FSGitStorage) catalogDelete(namespace, projectName, rel string) {
	c, err := s.catalogFor(namespace, projectName)
	if err != nil {
		s.log().Warn("catalog unavailable for delete_file",
			"namespace", namespace, "project", projectName, "path", rel, "err", err)
		s.catUpdateFailedDelete.Add(1)
		return
	}
	if derr := c.DeleteFile(rel); derr != nil {
		s.log().Warn("catalog delete failed for delete_file",
			"namespace", namespace, "project", projectName, "path", rel, "err", derr)
		s.catUpdateFailedDelete.Add(1)
	}
}

// CatalogStats reports the per-project file/dir counts for the metrics gauges
// (§10). Projects whose catalog cannot be opened are omitted.
type CatalogStats struct {
	Namespace string
	Project   string
	Files     int
	Dirs      int
}

// CatalogStatsAll returns Stats for every registered catalog.
func (s *FSGitStorage) CatalogStatsAll() []CatalogStats {
	s.catMu.Lock()
	handles := make(map[string]*catalog.Catalog, len(s.catalogs))
	for k, c := range s.catalogs {
		handles[k] = c
	}
	s.catMu.Unlock()

	out := make([]CatalogStats, 0, len(handles))
	for key, c := range handles {
		if c == nil {
			continue
		}
		st, err := c.Stats()
		if err != nil {
			continue
		}
		ns, proj := splitProjectKeyLocal(key)
		out = append(out, CatalogStats{Namespace: ns, Project: proj, Files: st.FileCount, Dirs: st.DirCount})
	}
	return out
}

// CatalogFileCounts returns each registered project's catalog file and
// directory counts keyed by "<namespace>/<project>" as [2]int{files, dirs}, for
// the metrics gauges (§10). Uses only primitives so the metrics package need
// not import storage.
func (s *FSGitStorage) CatalogFileCounts() map[string][2]int {
	stats := s.CatalogStatsAll()
	out := make(map[string][2]int, len(stats))
	for _, st := range stats {
		out[st.Namespace+"/"+st.Project] = [2]int{st.Files, st.Dirs}
	}
	return out
}

// CatalogCounters returns the catalog observability counters (§10), for the
// metrics Source.
func (s *FSGitStorage) CatalogCounters() (updateFailedWrite, updateFailedDelete, invariantViolations, rebuildMissing, rebuildCorrupt, rebuildSchema, rebuildUnreadable int64) {
	return s.catUpdateFailedWrite.Load(),
		s.catUpdateFailedDelete.Load(),
		s.catInvariantViolations.Load(),
		s.catRebuildMissing.Load(),
		s.catRebuildCorrupt.Load(),
		s.catRebuildSchema.Load(),
		s.catRebuildUnreadable.Load()
}

func splitProjectKeyLocal(key string) (namespace, project string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
