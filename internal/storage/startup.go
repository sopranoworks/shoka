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
	"github.com/sopranoworks/shoka/internal/storage/catalog"
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
// (D4 / B-38.1). treePath is the leftover directory; dbPaths are its present sibling DBs
// (catalog <project>.db, index <project>.index.db, deleted-log <project>.deleted.db) so they
// are relocated to lost+found WITH the dir rather than stranded as orphans.
type leftover struct {
	namespace string
	name      string
	treePath  string
	dbPaths   []string
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
		nsPath := filepath.Join(s.baseDir, ns.Name())
		projEntries, err := os.ReadDir(nsPath)
		if err != nil {
			continue
		}
		for _, pr := range projEntries {
			// classifyProjectEntry is the shared predicate ListProjects also routes
			// through (B-31): entrySkip = a "<project>.db" file or a Shoka-internal dot
			// dir; entryLeftover = a repo-less remnant; entryProject = a real project.
			switch classifyProjectEntry(nsPath, pr) {
			case entryLeftover:
				// Repo-less: not a project. Surface it (do not register it) so the
				// post-startup step relocates it to lost+found. Include EVERY present
				// derivative sibling (catalog/index/deleted-log via siblingDBPaths) so they
				// all move together rather than stranding as orphans.
				lf := leftover{namespace: ns.Name(), name: pr.Name(), treePath: filepath.Join(nsPath, pr.Name())}
				for _, p := range s.siblingDBPaths(ns.Name(), pr.Name()) {
					if _, statErr := os.Stat(p); statErr == nil {
						lf.dbPaths = append(lf.dbPaths, p)
					}
				}
				leftovers = append(leftovers, lf)
			case entryProject:
				out = append(out, projectRef{namespace: ns.Name(), name: pr.Name()})
			}
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

	// Auto-recover an interrupted SPECIAL op — move or ns/proj rename (B-28) — BEFORE
	// discovery/rescue: resume forward or roll back the journaled op so the managed set +
	// on-disk state are consistent before anything else reads them — no operator action.
	s.recoverInterruptedOp()

	projects, leftovers := s.discoverProjects()

	// Managed-namespace registry (B-28 stage A): the one-time rescue-adopt (only when the
	// registry has no managed info yet) + the always-managed `default`. Done before the
	// per-project init so the managed set is established for the rest of startup.
	s.reconcileManagedRegistry(projects)

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

	// One-time cleanup of OLD JUNK deleted-logs (2026-06-18): remove only unmarked + empty
	// <p>.deleted.db files left by the retired over-broad lazy-create. Runs here, inside the
	// blocking gate before the listeners open, so the server owns every handle (no live race);
	// guarded by a done-flag so it does NOT re-scan every boot, and it is O(1) per existing
	// .deleted.db (a marker lookup + an emptiness probe) — never a git walk, never a full scan.
	s.deletedLogCleanupOnce()

	// Non-blocking (D4 / B-38.1): quarantine repo-less leftovers to lost+found AFTER
	// the blocking gate has done its work. This runs in a goroutine so a whole-tree
	// move never delays server readiness — the synchronous body above (the latency
	// path callers gate listeners on) performs no relocation. It is idempotent-safe:
	// an interrupted relocation is simply retried on the next boot.
	if len(leftovers) > 0 {
		// relocWG lets Close (and in-package tests) await this goroutine's
		// completion (B-42). The wait happens off this synchronous body — never
		// here — so the readiness gate's latency stays free of the tree move.
		s.relocWG.Add(1)
		go func() {
			defer s.relocWG.Done()
			s.relocateLeftovers(leftovers, time.Now())
		}()
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
		// Re-confirm each sibling DB still exists (present at discovery); omit any now
		// absent so depositTree never fails on a missing sibling.
		var siblings []string
		for _, p := range lf.dbPaths {
			if _, err := os.Stat(p); err == nil {
				siblings = append(siblings, p)
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
			"tree", lf.treePath, "dbs", lf.dbPaths, "dest", dest)
		s.notifyQuarantined(lf.namespace, lf.name, lf.name)
	}
}

// reconcileManagedRegistry seeds the managed-namespace registry at startup. The one-time
// RESCUE-ADOPT runs ONLY when the registry is empty (no managed info yet — a deployment
// upgrading into the managed model, where real namespaces/projects exist on disk under the
// old "dir exists" model): every discovered .git project + its namespace is adopted as
// managed, so nothing real is stranded. It is idempotent — once the registry is non-empty
// it never re-runs (a later valid untracked dir is stage B's operator-driven manual adopt,
// not an automatic absorption). The adopt predicate is exactly discovery's: a dir is an
// adoptable project iff it has a .git repo (projects here are already classified entryProject
// by discoverProjects); repo-less leftovers and empty dirs are NOT adopted. The `default`
// namespace is then always ensured present (decision 3).
func (s *FSGitStorage) reconcileManagedRegistry(projects []projectRef) {
	if s.nsReg == nil {
		return
	}
	wasEmpty, err := s.nsReg.IsEmpty()
	if err != nil {
		s.log().Error("managed registry: emptiness check failed", "err", err)
	} else if wasEmpty {
		nsSet := make(map[string]bool)
		adopted := 0
		for _, p := range projects {
			if aerr := s.nsReg.AddProject(p.namespace, p.name); aerr != nil {
				s.log().Warn("managed registry: rescue-adopt failed",
					"namespace", p.namespace, "project", p.name, "err", aerr)
				continue
			}
			nsSet[p.namespace] = true
			adopted++
		}
		s.log().Info("managed registry: one-time rescue-adopt complete",
			"namespaces", len(nsSet), "projects", adopted)
	}
	// `default` is always managed (the namespace-omitted entry point), on every startup.
	if eerr := s.nsReg.EnsureNamespace(DefaultNamespace); eerr != nil {
		s.log().Error("managed registry: ensure default namespace failed", "err", eerr)
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
