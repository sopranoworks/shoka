package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoka/mcp-server/internal/storage/catalog"
)

// projectRef identifies a project on disk.
type projectRef struct {
	namespace string
	name      string
}

// discoverProjects walks <base_dir>/<namespace>/<project>/ and returns every
// project directory. Hidden namespace directories (e.g. ".shoka"), hidden
// project-level directories (e.g. the ".shoka-lostfound" area), and the
// per-project "<project>.db" catalog files (which are not directories) are
// skipped — a dot-prefixed entry is Shoka-internal, never a project.
func (s *FSGitStorage) discoverProjects() []projectRef {
	var out []projectRef
	nsEntries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log().Error("project discovery: cannot read base dir", "error", err)
		}
		return out
	}
	for _, ns := range nsEntries {
		if !ns.IsDir() || strings.HasPrefix(ns.Name(), ".") {
			continue
		}
		projEntries, err := os.ReadDir(filepath.Join(s.baseDir, ns.Name()))
		if err != nil {
			continue
		}
		for _, pr := range projEntries {
			if !pr.IsDir() || strings.HasPrefix(pr.Name(), ".") {
				continue // skips "<project>.db" files and Shoka-internal dot dirs
			}
			out = append(out, projectRef{namespace: ns.Name(), name: pr.Name()})
		}
	}
	return out
}

// StartupInit performs the blocking startup gate (directive §6): it drains the
// WAL into git, then for every project concurrently opens (or rebuilds) its
// catalog and computes its drift state. It returns only once every project is
// initialised, so callers can gate the MCP and Web UI listeners on it. A
// per-project failure marks that project dangerous and is logged; it never
// aborts startup.
func (s *FSGitStorage) StartupInit(ctx context.Context) {
	// Drain first: any pending WAL entries (from a prior run) are committed to git
	// before catalogs are rebuilt, so a rebuild-from-HEAD reflects every durable
	// write. (Deviation from §6's per-project "rebuild then drain" ordering, taken
	// because the WAL is global and the catalog is maintained synchronously on the
	// live write path — draining first strictly avoids a rebuilt catalog missing a
	// pending-but-uncommitted write. See the completion report.)
	s.WaitForWAL(2 * time.Minute)

	projects := s.discoverProjects()
	var wg sync.WaitGroup
	for _, p := range projects {
		wg.Add(1)
		go func(ns, name string) {
			defer wg.Done()
			s.initProject(ns, name)
		}(p.namespace, p.name)
	}
	wg.Wait()

	states := s.AllStates()
	var healthy, corrupted, dangerous int
	for _, st := range states {
		switch st {
		case StateHealthy:
			healthy++
		case StateCorrupted:
			corrupted++
		case StateDangerous:
			dangerous++
		}
	}
	s.log().Info("startup catalog init complete",
		"projects_total", len(projects),
		"projects_healthy", healthy,
		"projects_corrupted", corrupted,
		"projects_dangerous", dangerous,
	)
}

// initProject opens a single project's catalog (rebuilding from git HEAD if it
// is missing, corrupt, or schema-mismatched), registers it, then runs drift
// detection to set the project's state.
func (s *FSGitStorage) initProject(namespace, projectName string) {
	cat, err := catalog.Open(s.catalogPath(namespace, projectName))
	if err != nil {
		s.countRebuildReason(err)
		s.log().Warn("catalog open failed, rebuilding from git HEAD",
			"namespace", namespace, "project", projectName, "reason", rebuildReason(err), "err", err)
		if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
			s.setState(namespace, projectName, StateDangerous)
			s.log().Error("rebuild catalog failed; project marked dangerous",
				"namespace", namespace, "project", projectName, "err", rerr)
			return
		}
	} else {
		s.registerCatalog(namespace, projectName, cat)
	}

	if _, derr := s.DetectDrift(namespace, projectName); derr != nil {
		s.log().Error("startup drift detection failed",
			"namespace", namespace, "project", projectName, "err", derr)
	}
}

// rebuildReason classifies a catalog.Open error into a metric label.
func rebuildReason(err error) string {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return "missing"
	case errors.Is(err, catalog.ErrSchemaMismatch):
		return "schema_mismatch"
	case errors.Is(err, catalog.ErrCorrupt):
		return "corrupt"
	default:
		return "unreadable"
	}
}

// countRebuildReason increments the rebuild counter matching the open error.
func (s *FSGitStorage) countRebuildReason(err error) {
	switch rebuildReason(err) {
	case "missing":
		s.catRebuildMissing.Add(1)
	case "schema_mismatch":
		s.catRebuildSchema.Add(1)
	case "corrupt":
		s.catRebuildCorrupt.Add(1)
	default:
		s.catRebuildUnreadable.Add(1)
	}
}

// rebuildAndRegister closes and unregisters any open handle for the project
// (so the .db file can be replaced), rebuilds it from git HEAD, and registers
// the new handle.
func (s *FSGitStorage) rebuildAndRegister(namespace, projectName string) error {
	key := projectKey(namespace, projectName)
	s.catMu.Lock()
	if old, ok := s.catalogs[key]; ok && old != nil {
		_ = old.Close()
		delete(s.catalogs, key)
	}
	s.catMu.Unlock()

	cat, err := s.rebuildCatalog(namespace, projectName)
	if err != nil {
		return err
	}
	s.registerCatalog(namespace, projectName, cat)
	return nil
}

// rebuildCatalog rebuilds a project's catalog from git HEAD (directive §7). It
// removes any existing DB file, creates a fresh catalog, and inserts one entry
// per HEAD path using the WORKING TREE file's content hash, size, and mtime. A
// HEAD path that is missing from the working tree is logged and the project is
// marked corrupted (the rare case needing operator attention). A repository
// with no commits yields an empty catalog.
func (s *FSGitStorage) rebuildCatalog(namespace, projectName string) (*catalog.Catalog, error) {
	p := s.catalogPath(namespace, projectName)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale catalog: %w", err)
	}
	cat, err := catalog.Create(p, namespace, projectName)
	if err != nil {
		return nil, fmt.Errorf("create catalog: %w", err)
	}

	projectPath := filepath.Join(s.baseDir, namespace, projectName)
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("open git: %w", err)
	}
	ref, err := r.Head()
	if err != nil {
		// No commits yet: an empty catalog is the correct initial state.
		return cat, nil
	}
	commit, err := r.CommitObject(ref.Hash())
	if err != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("resolve HEAD commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("resolve HEAD tree: %w", err)
	}

	corrupted := false
	walkErr := tree.Files().ForEach(func(f *object.File) error {
		full := filepath.Join(projectPath, filepath.FromSlash(f.Name))
		info, serr := os.Stat(full)
		if serr != nil {
			// HEAD references a path absent from the working tree: genuine trouble.
			s.log().Error("rebuild: HEAD path missing from working tree",
				"namespace", namespace, "project", projectName, "path", f.Name)
			corrupted = true
			return nil
		}
		data, rerr := os.ReadFile(full)
		if rerr != nil {
			corrupted = true
			return nil
		}
		return cat.PutFile(f.Name, catalog.FileEntry{
			Etag:       sha256Hex(data),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
	})
	if walkErr != nil {
		_ = cat.Close()
		return nil, fmt.Errorf("walk HEAD tree: %w", walkErr)
	}
	if corrupted {
		s.setState(namespace, projectName, StateCorrupted)
	}
	return cat, nil
}
