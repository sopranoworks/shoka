package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Namespace health (B-28 ns/proj-management stage B): the check that compares the managed
// registry ("what should be" — stage A) against on-disk + catalog + git ("what is"), plus
// the per-divergence recovery actions. Health is the namespace-level lift of the c9f6827
// project-level lesson: a managed thing that is missing or broken is UNHEALTHY; a foreign
// dir Shoka does not manage is IGNORABLE (it never fails health, though a valid one is
// surfaced as operator-adoptable). The check is READ-ONLY in the "what should be" sense —
// it never drops a registry record, deletes a dir, or adopts; the only side effect is the
// existing c9f6827 catalog re-sync that DetectDrift performs for a stale-but-clean project
// (non-destructive healing, reused per the directive, not reinvented).

const projectStateMissing = "missing" // a managed project the registry records but no .git dir exists for

// ProjectHealth is one managed project's health: its name and state — "healthy" /
// "corrupted" / "dangerous" (the c9f6827 per-project verdict) or "missing" (registry has
// it, the on-disk .git project is gone).
type ProjectHealth struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// ForeignDir is a directory Shoka does NOT manage (not in the registry). Adoptable is true
// when it is a valid project/namespace by the stage-A .git predicate (so the operator MAY
// adopt it); foreign dirs never make a namespace unhealthy.
type ForeignDir struct {
	Name      string `json:"name"`
	Adoptable bool   `json:"adoptable"`
}

// OrphanSibling is a stray catalog/index/deleted-log/vector .db (e.g. <proj>.db /
// <proj>.index.db / <proj>.deleted.db / <proj>.vector.db) whose project dir is gone — a managed thing left
// broken/incomplete (UNHEALTHY). Name is the project base name the stray DBs belong to;
// Files are the actual on-disk sibling filenames so the UI can show the FULL filename
// (e.g. "shoka.db") rather than the bare base (which collided confusingly with a namespace
// name in the field).
type OrphanSibling struct {
	Name  string   `json:"name"`
	Files []string `json:"files,omitempty"`
}

// NamespaceHealth is one managed namespace's health picture.
type NamespaceHealth struct {
	Name     string          `json:"name"`
	Present  bool            `json:"present"` // the namespace directory exists on disk
	Healthy  bool            `json:"healthy"` // no missing/corrupted/orphaned managed thing
	Projects []ProjectHealth `json:"projects"`
	Foreign  []ForeignDir    `json:"foreign,omitempty"`  // untracked dirs inside this namespace
	Orphaned []OrphanSibling `json:"orphaned,omitempty"` // stray catalog/index DBs
}

// HealthReport is the whole managed picture plus base-level foreign namespaces.
type HealthReport struct {
	Namespaces        []NamespaceHealth `json:"namespaces"`
	ForeignNamespaces []ForeignDir      `json:"foreign_namespaces,omitempty"`
}

// CheckAllHealth reconciles the entire managed set against disk/catalog/git and returns the
// structured health picture. It is read-only (it never mutates the registry or removes
// anything; DetectDrift's c9f6827 stale-catalog re-sync is the only — non-destructive —
// side effect). Surfacing layers filter the result by the principal's admin scope.
func (s *FSGitStorage) CheckAllHealth() HealthReport {
	var report HealthReport
	if s.nsReg == nil {
		return report
	}
	managed, _ := s.nsReg.List()
	managedSet := make(map[string]bool, len(managed))
	for _, ns := range managed {
		managedSet[ns] = true
		report.Namespaces = append(report.Namespaces, s.checkNamespaceHealth(ns))
	}
	// Foreign namespaces: non-hidden base-dir subdirs not in the registry. Adoptable iff
	// the dir contains ≥1 .git project (the stage-A namespace adopt predicate).
	if entries, err := os.ReadDir(s.baseDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || managedSet[e.Name()] {
				continue
			}
			report.ForeignNamespaces = append(report.ForeignNamespaces, ForeignDir{
				Name:      e.Name(),
				Adoptable: s.namespaceHasProject(e.Name()),
			})
		}
	}
	sort.Slice(report.ForeignNamespaces, func(i, j int) bool {
		return report.ForeignNamespaces[i].Name < report.ForeignNamespaces[j].Name
	})
	return report
}

// CheckNamespaceHealth returns the health of a single managed namespace.
func (s *FSGitStorage) CheckNamespaceHealth(namespace string) NamespaceHealth {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return s.checkNamespaceHealth(namespace)
}

func (s *FSGitStorage) checkNamespaceHealth(ns string) NamespaceHealth {
	nh := NamespaceHealth{Name: ns, Healthy: true}
	rec, _, _ := s.nsReg.Get(ns)
	managedProj := make(map[string]bool, len(rec.Projects))
	for _, p := range rec.Projects {
		managedProj[p] = true
	}

	nsDir := filepath.Join(s.baseDir, ns)
	info, statErr := os.Stat(nsDir)
	nh.Present = statErr == nil && info.IsDir()

	if !nh.Present {
		// The directory is absent. Its managed projects (if any) are MISSING and the
		// namespace is unhealthy. A managed-but-empty namespace with no on-disk footprint
		// yet (e.g. `default` before any project) has nothing missing → still healthy.
		for _, p := range rec.Projects {
			nh.Projects = append(nh.Projects, ProjectHealth{Name: p, State: projectStateMissing})
			nh.Healthy = false
		}
		sortProjects(nh.Projects)
		return nh
	}

	// Enumerate the namespace directory once.
	projDirs := make(map[string]bool)    // names that ARE .git projects
	dbBases := make(map[string][]string) // base -> the actual sibling .db filenames present
	if entries, err := os.ReadDir(nsDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				if strings.HasPrefix(name, ".") {
					continue // Shoka-internal (.git/.shoka-lostfound/…)
				}
				if hasGitRepo(filepath.Join(nsDir, name)) {
					projDirs[name] = true
				} else {
					// A repo-less untracked dir: foreign, not adoptable as a project.
					nh.Foreign = append(nh.Foreign, ForeignDir{Name: name, Adoptable: false})
				}
				continue
			}
			if base, ok := dbBaseName(name); ok {
				dbBases[base] = append(dbBases[base], name)
			}
		}
	}

	// Managed projects: present (.git) → the c9f6827 per-project verdict; absent → MISSING.
	for _, p := range rec.Projects {
		if projDirs[p] {
			sum, derr := s.DetectDrift(ns, p)
			st := string(sum.State)
			if derr != nil {
				st = string(StateDangerous)
			}
			nh.Projects = append(nh.Projects, ProjectHealth{Name: p, State: st})
			if sum.State != StateHealthy {
				nh.Healthy = false
			}
		} else {
			nh.Projects = append(nh.Projects, ProjectHealth{Name: p, State: projectStateMissing})
			nh.Healthy = false
		}
	}

	// Foreign projects: .git dirs that are not managed (adoptable).
	for name := range projDirs {
		if !managedProj[name] {
			nh.Foreign = append(nh.Foreign, ForeignDir{Name: name, Adoptable: true})
		}
	}

	// Orphaned siblings: a catalog/index/deleted-log/vector .db whose project dir is gone
	// — UNHEALTHY. A LIVE project's siblings (its <p>.db, <p>.index.db, <p>.deleted.db,
	// AND <p>.vector.db, all mapped to base <p> by dbBaseName) are NOT orphaned, because
	// projDirs[<p>] is set. Files carries the real filenames so the UI shows the full name.
	for base, files := range dbBases {
		if !projDirs[base] {
			sort.Strings(files)
			nh.Orphaned = append(nh.Orphaned, OrphanSibling{Name: base, Files: files})
			nh.Healthy = false
		}
	}

	sortProjects(nh.Projects)
	sort.Slice(nh.Foreign, func(i, j int) bool { return nh.Foreign[i].Name < nh.Foreign[j].Name })
	sort.Slice(nh.Orphaned, func(i, j int) bool { return nh.Orphaned[i].Name < nh.Orphaned[j].Name })
	return nh
}

func sortProjects(ps []ProjectHealth) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
}

// dbBaseName returns the project base name of a per-project sibling DB file and true;
// ("", false) for any other file. Shoka keeps FOUR derivative siblings per project:
// the catalog <proj>.db, the index <proj>.index.db, the deleted-log <proj>.deleted.db,
// and the vector index <proj>.vector.db. The longer compound suffixes MUST be checked
// before the bare ".db" — otherwise "<proj>.vector.db" would map to base "<proj>.vector"
// (a phantom with no project dir) and a LIVE project's vector index would be falsely
// flagged orphaned (and Clean would delete it). All four map to base <proj>.
func dbBaseName(fileName string) (string, bool) {
	if strings.HasSuffix(fileName, ".index.db") {
		return strings.TrimSuffix(fileName, ".index.db"), true
	}
	if strings.HasSuffix(fileName, ".deleted.db") {
		return strings.TrimSuffix(fileName, ".deleted.db"), true
	}
	if strings.HasSuffix(fileName, ".vector.db") {
		return strings.TrimSuffix(fileName, ".vector.db"), true
	}
	if strings.HasSuffix(fileName, ".db") {
		return strings.TrimSuffix(fileName, ".db"), true
	}
	return "", false
}

// namespaceHasProject reports whether a directory <base>/<ns> contains ≥1 .git project —
// the stage-A predicate for an adoptable namespace.
func (s *FSGitStorage) namespaceHasProject(ns string) bool {
	entries, err := os.ReadDir(filepath.Join(s.baseDir, ns))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if classifyProjectEntry(filepath.Join(s.baseDir, ns), e) == entryProject {
			return true
		}
	}
	return false
}

// --- recovery actions (each EXPLICIT and non-destructive by default) ---------

// DropMissingProject removes a managed project's registry record when its on-disk .git
// project is confirmed absent (the MISSING case). It is NEVER called automatically by the
// health check — a transient mount/disk failure must not delete the managed record of real
// data; only an explicit operator request runs it. It refuses if the project is in fact
// present (use DeleteProject to remove a present project). It touches only the registry,
// not disk (a stray sibling .db is the separate CleanOrphanedSibling concern).
func (s *FSGitStorage) DropMissingProject(namespace, projectName string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if hasGitRepo(filepath.Join(s.baseDir, namespace, projectName)) {
		return fmt.Errorf("project %s/%s is present on disk; refusing to drop its managed record", namespace, projectName)
	}
	if s.nsReg == nil {
		return nil
	}
	return s.nsReg.RemoveProject(namespace, projectName)
}

// DropMissingNamespace removes a managed namespace's registry record when its on-disk
// directory is confirmed absent. NEVER automatic; refuses a present namespace (use
// DeleteNamespace) and refuses the delete-protected default namespace.
func (s *FSGitStorage) DropMissingNamespace(namespace string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if namespace == DefaultNamespace {
		return fmt.Errorf("the %q namespace cannot be dropped (it is the default entry point)", DefaultNamespace)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, namespace)); err == nil {
		return fmt.Errorf("namespace %s is present on disk; refusing to drop its managed record", namespace)
	}
	if s.nsReg == nil {
		return nil
	}
	return s.nsReg.RemoveNamespace(namespace)
}

// RecoverCorruptedProject re-syncs a corrupted project's write-path baseline to the live
// git HEAD — the existing c9f6827 ResyncToHead/recover_project path, not a reinvention. It
// is non-destructive (neither commits nor discards); genuine drift stays corrupted.
func (s *FSGitStorage) RecoverCorruptedProject(namespace, projectName string) (ProjectState, error) {
	return s.ResyncToHead(namespace, projectName)
}

// CleanOrphanedSibling removes stray sibling .db files (<name>.db / <name>.index.db /
// <name>.deleted.db / <name>.vector.db) that have no project directory — the ORPHANED case.
// It refuses when a live .git project of that name exists (so a present project's catalog
// is never deleted), and removes only the stray sibling DBs (the part-1 atomic-delete
// discipline), evicting any in-memory handle first. Explicit operator action only.
func (s *FSGitStorage) CleanOrphanedSibling(namespace, name string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if hasGitRepo(filepath.Join(s.baseDir, namespace, name)) {
		return fmt.Errorf("%s/%s is a live project; refusing to clean its catalog/index as orphaned", namespace, name)
	}
	// DATA-LOSS GUARD: refuse to clean a LIVE project's derivative sibling. A name like
	// "<p>.deleted" or "<p>.index" would, via siblingDBPaths, resolve to <p>.deleted.db /
	// <p>.index.db — the live deleted-log / index of project <p> — and deleting it is silent
	// data loss. The dbBaseName fix means the health check no longer surfaces such an item,
	// but a stale UI or direct call could still pass it; refuse defensively. (A genuine
	// stray whose base is NOT a live project still cleans normally.)
	for _, suf := range []string{".deleted", ".index", ".vector"} {
		if base := strings.TrimSuffix(name, suf); base != name && hasGitRepo(filepath.Join(s.baseDir, namespace, base)) {
			return fmt.Errorf("%s/%s is a live project's %q sibling; refusing to clean (would delete live data)", namespace, base, suf)
		}
	}
	s.evictProjectHandles(namespace, name)
	// Remove every derivative sibling of the stray base (catalog/index/deleted-log) via the
	// single siblingDBPaths source of truth, so cleaning a stray never leaves one behind.
	var errs []error
	for _, p := range s.siblingDBPaths(namespace, name) {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove sibling db %s: %w", filepath.Base(p), err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// AdoptForeign brings a valid untracked namespace/project under management — the explicit,
// operator-driven adopt of an UNTRACKED-FOREIGN dir (managed info now exists, so this is
// never automatic; the auto-rescue only ran in stage A when the registry was empty). With
// a project name it adopts that project (which must be a valid .git project), auto-
// registering its parent namespace. With an empty project name it adopts the namespace and
// every .git project under it (the stage-A predicate). It refuses a target that is not a
// valid project/namespace, so genuine foreign junk is never absorbed. Idempotent on an
// already-managed target.
func (s *FSGitStorage) AdoptForeign(namespace, projectName string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if s.nsReg == nil {
		return nil
	}
	if projectName != "" {
		if !hasGitRepo(filepath.Join(s.baseDir, namespace, projectName)) {
			return fmt.Errorf("%s/%s is not a valid (.git) project; refusing to adopt", namespace, projectName)
		}
		return s.nsReg.AddProject(namespace, projectName)
	}
	// Namespace adopt: it must exist on disk and contain ≥1 .git project.
	if !s.namespaceHasProject(namespace) {
		return fmt.Errorf("namespace %s has no valid (.git) project; refusing to adopt", namespace)
	}
	if err := s.nsReg.EnsureNamespace(namespace); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.baseDir, namespace))
	if err != nil {
		return fmt.Errorf("read namespace dir for adopt: %w", err)
	}
	for _, e := range entries {
		if classifyProjectEntry(filepath.Join(s.baseDir, namespace), e) == entryProject {
			if aerr := s.nsReg.AddProject(namespace, e.Name()); aerr != nil {
				return aerr
			}
		}
	}
	return nil
}
