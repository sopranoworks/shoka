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

// leftover is a repo-less <namespace>/<project> directory surfaced (not dropped)
// by discoverProjects: a directory with no .git is not a project but a remnant — a
// pre-B-37 guard-less write half-created one (dir + per-project .db, no repo), or an
// externally-created stray. It is never registered; the non-blocking
// post-StartupInit relocation step (relocateLeftovers) quarantines it to lost+found
// (D4 / B-38.1). treePath is the leftover directory; dbPath is its sibling
// <project>.db when present, otherwise "".
type leftover struct {
	namespace string
	name      string
	treePath  string
	dbPath    string
}

// discoverProjects walks <base_dir>/<namespace>/<project>/ and returns every real,
// git-backed project (the primary result) plus every repo-less leftover directory
// (the second result). Hidden namespace directories (e.g. ".shoka"), hidden
// project-level directories (e.g. the ".shoka-lostfound" area), and the per-project
// "<project>.db" catalog files (which are not directories) are skipped — a
// dot-prefixed entry is Shoka-internal, never a project.
//
// A <project> directory with no .git is not a project; rather than being silently
// dropped (the B-37 minimum: it must never be registered, or it re-adopts a phantom
// every boot and loops the WAL worker) it is SURFACED on the leftovers list so the
// post-startup relocation step (relocateLeftovers) can quarantine it to lost+found
// (D4). The primary project result is unchanged, and the other two callers
// (scanAllProjects, sweepAllProjects) ignore the leftovers. Discovery does NOT log
// the leftover: it runs in three callers (incl. the default-on lost+found sweep), so
// the operator-facing record is emitted once at the relocation step, not on every
// scan.
func (s *FSGitStorage) discoverProjects() ([]projectRef, []leftover) {
	var out []projectRef
	var leftovers []leftover
	nsEntries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.log().Error("project discovery: cannot read base dir", "error", err)
		}
		return out, leftovers
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
			projectPath := filepath.Join(s.baseDir, ns.Name(), pr.Name())
			if !hasGitRepo(projectPath) {
				// Repo-less: not a project. Surface it (do not register it) so the
				// post-startup step relocates it to lost+found. Include the sibling
				// "<project>.db" when it is present so the two move together.
				lf := leftover{namespace: ns.Name(), name: pr.Name(), treePath: projectPath}
				dbPath := s.catalogPath(ns.Name(), pr.Name())
				if _, statErr := os.Stat(dbPath); statErr == nil {
					lf.dbPath = dbPath
				}
				leftovers = append(leftovers, lf)
				continue
			}
			out = append(out, projectRef{namespace: ns.Name(), name: pr.Name()})
		}
	}
	return out, leftovers
}

// hasGitRepo reports whether projectPath is a real, git-backed project: it has a
// .git entry. CreateProject git-inits every legitimate project, so a .git-less
// directory is not a project but leftover (a pre-B-37 guard-less write half-created
// one). Used by the write-path guard (checkWritable) and discovery, so neither the
// mutation path nor catalog init ever treats a repo-less directory as a project.
func hasGitRepo(projectPath string) bool {
	_, err := os.Stat(filepath.Join(projectPath, ".git"))
	return err == nil
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

	projects, leftovers := s.discoverProjects()
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

	// Non-blocking (D4 / B-38.1): quarantine repo-less leftovers to lost+found AFTER
	// the blocking gate has done its work. This runs in a goroutine so a whole-tree
	// move never delays server readiness — the synchronous body above (the latency
	// path callers gate listeners on) performs no relocation. It is idempotent-safe:
	// an interrupted relocation is simply retried on the next boot.
	if len(leftovers) > 0 {
		go s.relocateLeftovers(leftovers, time.Now())
	}
}

// relocateLeftovers quarantines each repo-less leftover surfaced by discoverProjects
// to lost+found (D4 / B-38.1). It is spawned as a goroutine by StartupInit so the
// blocking startup gate's latency is unchanged. For each leftover it moves the whole
// <namespace>/<project> tree and its sibling <project>.db (when present) into one
// lost+found <ts> directory via D2's depositTree, then emits a lostfound.quarantined
// NOTIFY (the single operator-facing record — discovery itself stays silent). It is
// idempotent-safe: a leftover whose tree is already gone (relocated on a prior boot,
// or a crash-interrupted move) is skipped, not errored; a missing .db is omitted. It
// writes no git ref (Anchor 3 N/A) and touches only the namespace-root lost+found
// area, never project history.
func (s *FSGitStorage) relocateLeftovers(leftovers []leftover, now time.Time) {
	for _, lf := range leftovers {
		// Idempotency / interrupted-move guard: if the tree is already gone there is
		// nothing to relocate (a prior boot moved it, or a crash landed mid-move).
		if _, err := os.Stat(lf.treePath); err != nil {
			if !os.IsNotExist(err) {
				s.log().Error("leftover relocation: cannot stat leftover tree",
					"namespace", lf.namespace, "project", lf.name, "path", lf.treePath, "error", err)
			}
			continue
		}
		// Re-confirm the sibling .db still exists (it was present at discovery); omit
		// it gracefully if absent so depositTree never fails on a missing sibling.
		var siblings []string
		if lf.dbPath != "" {
			if _, err := os.Stat(lf.dbPath); err == nil {
				siblings = append(siblings, lf.dbPath)
			}
		}
		dest, err := s.depositTree(lf.namespace, lf.name, lf.treePath, now, siblings...)
		if err != nil {
			s.log().Error("leftover relocation: move to lost+found failed",
				"namespace", lf.namespace, "project", lf.name, "path", lf.treePath, "error", err)
			continue
		}
		s.log().Warn("relocated repo-less leftover to lost+found",
			"namespace", lf.namespace, "project", lf.name,
			"tree", lf.treePath, "db", lf.dbPath, "dest", dest)
		s.notifyQuarantined(lf.namespace, lf.name, lf.name)
	}
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
