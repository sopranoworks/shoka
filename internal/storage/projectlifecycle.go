package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/utils"
)

// ScopeCleaner removes authorization grants that reference a deleted namespace or
// project BY NAME (the B-28 namespace/project-management cascade cleanup). It is wired at
// composition time (cmd/shoka) over the userstore (accounts + pending invites) and the
// optional oauthstore (issued token series); storage holds only this interface so the
// go-git layer never imports the auth stores. The invariant it enforces: after a
// namespace/project is deleted, no persisted scope still references it, so re-creating
// the same name does NOT resurrect old access (leave-graceful is rejected).
type ScopeCleaner interface {
	// PurgeNamespace removes every grant referencing namespace ns (the namespace-wide
	// grant and any project under it) from every persisted scope.
	PurgeNamespace(ns string) error
	// PurgeProject removes every grant referencing the specific project ns/proj from
	// every persisted scope (namespace-wide and wildcard grants are left intact).
	PurgeProject(ns, proj string) error
	// RewriteProject re-homes every grant referencing project oldNs/oldProj to newNs/newProj
	// across every persisted scope — serving BOTH a project move (oldProj==newProj, ns
	// changes) and a project rename (oldNs==newNs, proj changes). Namespace-wide and wildcard
	// grants are left intact.
	RewriteProject(oldNs, oldProj, newNs, newProj string) error
	// RewriteNamespace re-homes every grant referencing namespace old to new — BOTH the
	// namespace-wide grant AND every project-specific grant under it — across every persisted
	// scope (the namespace-rename mirror of PurgeNamespace; wildcard grants left intact).
	RewriteNamespace(old, new string) error
}

// registerManagedProject records proj under namespace in the managed registry, auto-
// registering the parent namespace if absent (the CreateProject safety-net path, decision
// 5). It returns an error so CreateProject fails if the project cannot be brought under
// management — a project on disk but absent from the managed set is exactly the
// inconsistency the registry exists to prevent.
func (s *FSGitStorage) registerManagedProject(namespace, projectName string) error {
	if s.nsReg == nil {
		return nil
	}
	if err := s.nsReg.AddProject(namespace, projectName); err != nil {
		return fmt.Errorf("register managed project %s/%s: %w", namespace, projectName, err)
	}
	return nil
}

// SetScopeCleaner installs the cascade-cleanup hook DeleteProject/DeleteNamespace call
// after a delete. Passing nil disables cascade cleanup (the delete then performs only the
// on-disk removal) — the default for storage built without the auth stores (tests).
func (s *FSGitStorage) SetScopeCleaner(c ScopeCleaner) { s.scopeCleaner = c }

// CreateNamespace makes an explicit, EMPTY namespace: it creates the directory
// <base>/<ns> so the namespace is enumerated by ListNamespaces even with zero projects,
// and (because ListNamespaces no longer requires a project) it SURVIVES deleting its last
// project — only DeleteNamespace removes it. Idempotent: creating an existing namespace
// is a no-op success (mirroring CreateProjectCtx's already-exists handling). The name is
// validated with utils.IsValidName (so the wildcard sentinel "*" can never be a real
// namespace — the authz super-user gate depends on that).
func (s *FSGitStorage) CreateNamespace(namespace string) error {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return fmt.Errorf("invalid namespace: %s", namespace)
	}
	if err := os.MkdirAll(filepath.Join(s.baseDir, namespace), 0o755); err != nil {
		return fmt.Errorf("failed to create namespace directory: %w", err)
	}
	// Register it in the managed set (B-28 stage A) — this, not the bare dir, is what makes
	// the namespace appear in ListNamespaces. Idempotent.
	if s.nsReg != nil {
		if err := s.nsReg.EnsureNamespace(namespace); err != nil {
			return fmt.Errorf("register managed namespace %s: %w", namespace, err)
		}
	}
	return nil
}

// DefaultNamespace is the namespace-omitted entry point ("create a project without
// thinking about namespaces"; every MCP tool defaults `namespace` to it). It is ALWAYS
// managed (ensured at startup) and is delete-protected — DeleteNamespace refuses it — so
// the default entry point can never vanish from the managed set.
const DefaultNamespace = "default"

// DeleteProject permanently removes an entire project — its working tree + git repo AND
// both sibling derivative DBs (the catalog <proj>.db and the index <proj>.index.db) — and
// evicts the in-memory handles, then cascade-cleans every authorization grant that
// referenced it by name. It is the destructive admin op behind the management UI's
// project delete (B-28 part 1), and the riskiest unit in this change.
//
// Atomicity / sibling-safety (the c9f6827 substrate): the catalog and index live as
// SIBLING files beside the project dir, not inside it, so removing only the dir would
// strand them and the discovery sweep would flag a leftover. All three are removed
// together, and only this project's in-memory handles (catalogs/indexes/states) are
// evicted — a sibling project in the same namespace is never touched.
//
// Ordering: cascade-clean grants FIRST (a reliable, idempotent bbolt mutation), so a
// cleanup failure aborts before any on-disk removal, and a successful cleanup followed by
// a failed removal only REDUCES access (fail-safe) — it never strands a grant for a
// deleted name. Reads take no lock, so an in-flight read is not fenced; the on-disk
// removal happens LAST, so such a read either completes against the still-present files or
// sees not-found. A write in progress IS fenced (refuse-while-locked).
func (s *FSGitStorage) DeleteProject(ctx context.Context, namespace, projectName string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	projectPath, err := s.getProjectPath(namespace, projectName) // validates names
	if err != nil {
		return err
	}

	// Fence: refuse while any write lock is held on a path within this project.
	if s.projectHasActiveLease(projectPath) {
		return fmt.Errorf("cannot delete project %s/%s: a write is in progress", namespace, projectName)
	}

	// Cascade-clean grants first (fail-safe ordering — see the doc above).
	if s.scopeCleaner != nil {
		if cerr := s.scopeCleaner.PurgeProject(namespace, projectName); cerr != nil {
			return fmt.Errorf("cascade-clean project grants for %s/%s: %w", namespace, projectName, cerr)
		}
	}

	// Evict the in-memory handles FIRST (close the bbolt catalog + index, drop the state)
	// so the .db sibling files can be unlinked with no open handle.
	s.evictProjectHandles(namespace, projectName)

	// Remove the project dir AND every derivative sibling DB together (catalog, index,
	// deleted-log — via the single siblingDBPaths source of truth, so a newly-added sibling
	// is never forgotten here). Collect errors so a partial failure is surfaced rather than
	// silently leaving a sibling behind.
	var errs []error
	if rerr := os.RemoveAll(projectPath); rerr != nil {
		errs = append(errs, fmt.Errorf("remove project dir: %w", rerr))
	}
	for _, p := range s.siblingDBPaths(namespace, projectName) {
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			errs = append(errs, fmt.Errorf("remove sibling db %s: %w", filepath.Base(p), rerr))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// project_deleted change event + notification (mirrors the project_created emit in
	// CreateProjectCtx) so open /ws/ui subscriptions viewing this project are told it is
	// gone. The ctx-borne sender excludes the originator from its own event.
	s.notify.NotifyFrom(notify.SenderFrom(ctx), "project.delete", namespace+"/"+projectName, "")
	s.emit(ChangeEvent{
		Event:     "project_deleted",
		Namespace: namespace,
		Project:   projectName,
		Timestamp: time.Now(),
	})
	// Deregister the project from the managed set (B-28 stage A). The namespace record
	// itself stays (a namespace survives the deletion of its last project). Best-effort
	// after the on-disk removal: the project IS gone, so a registry hiccup must not fail
	// the delete; stage B's health check reconciles any residual drift.
	if s.nsReg != nil {
		if err := s.nsReg.RemoveProject(namespace, projectName); err != nil {
			s.log().Warn("deregister managed project failed",
				"namespace", namespace, "project", projectName, "err", err)
		}
	}
	return nil
}

// DeleteNamespace removes a managed namespace, but ONLY WHEN IT IS EMPTY (B-28 part 2): it
// refuses a namespace that still holds any project — the operator must delete the projects
// one at a time first (each its own high-friction confirm). It then cascade-cleans any
// namespace-wide grants, removes the empty namespace directory, and deregisters it.
// super-user-only at the call sites; the `default` namespace is delete-protected. ctx is
// retained for signature symmetry with the project ops (no fan-out runs under it now).
func (s *FSGitStorage) DeleteNamespace(ctx context.Context, namespace string) error {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return fmt.Errorf("invalid namespace: %s", namespace)
	}
	// The `default` namespace is the namespace-omitted entry point and is delete-protected
	// (decision 3): refuse to remove it so it can never vanish from the managed set.
	if namespace == DefaultNamespace {
		return fmt.Errorf("the %q namespace cannot be deleted (it is the default entry point)", DefaultNamespace)
	}
	// EMPTY-ONLY (B-28 part 2): an irreversible namespace delete must be deliberate, so it
	// refuses a non-empty namespace — the operator deletes each project one at a time first
	// (each its own high-friction confirm). This replaces part 1's fan-out mass delete. The
	// server refusal is the authoritative guard; the UI's disabled control is only UX.
	projects, err := s.ListProjects(namespace)
	if err != nil {
		return fmt.Errorf("list projects for namespace delete: %w", err)
	}
	if len(projects) > 0 {
		return fmt.Errorf("namespace %s is not empty (%d project(s)); delete its projects first", namespace, len(projects))
	}
	// Cascade-clean any namespace-wide grants (namespace:<ns>) before removing the dir.
	if s.scopeCleaner != nil {
		if cerr := s.scopeCleaner.PurgeNamespace(namespace); cerr != nil {
			return fmt.Errorf("cascade-clean namespace grants for %s: %w", namespace, cerr)
		}
	}
	if rerr := os.RemoveAll(filepath.Join(s.baseDir, namespace)); rerr != nil {
		return fmt.Errorf("remove namespace dir: %w", rerr)
	}
	// Deregister the namespace from the managed set (B-28 stage A). Best-effort after the
	// on-disk removal (the data is gone; stage B reconciles any residual drift).
	if s.nsReg != nil {
		if err := s.nsReg.RemoveNamespace(namespace); err != nil {
			s.log().Warn("deregister managed namespace failed", "namespace", namespace, "err", err)
		}
	}
	return nil
}

// evictProjectHandles closes and unregisters a project's in-memory catalog, index, AND
// deleted-log handles and drops its health state, so the on-disk .db siblings can be
// unlinked/relocated with no open handle and no stale entry survives. It touches only this
// project's keys.
func (s *FSGitStorage) evictProjectHandles(namespace, projectName string) {
	key := projectKey(namespace, projectName)
	s.catMu.Lock()
	if c, ok := s.catalogs[key]; ok {
		if c != nil {
			if err := c.Close(); err != nil {
				s.log().Warn("catalog close on delete failed", "project", key, "err", err)
			}
		}
		delete(s.catalogs, key)
	}
	s.catMu.Unlock()
	s.idxMu.Lock()
	if ix, ok := s.indexes[key]; ok {
		if ix != nil {
			if err := ix.Close(); err != nil {
				s.log().Warn("index close on delete failed", "project", key, "err", err)
			}
		}
		delete(s.indexes, key)
	}
	s.idxMu.Unlock()
	// The deleted-log is the third per-project sibling (2026-06-18); close its handle too so
	// its <proj>.deleted.db can be unlinked/relocated with no open handle.
	s.dlMu.Lock()
	if dl, ok := s.deletedLogs[key]; ok {
		if dl != nil {
			if err := dl.Close(); err != nil {
				s.log().Warn("deleted-log close on delete failed", "project", key, "err", err)
			}
		}
		delete(s.deletedLogs, key)
	}
	s.dlMu.Unlock()
	s.stateMu.Lock()
	delete(s.states, key)
	s.stateMu.Unlock()
}

// projectHasActiveLease reports whether any held write lease covers a path within the
// project (leases are keyed by the joined full path). Reads take no lock and are not
// covered — the delete removes files LAST so an in-flight read tolerates it.
func (s *FSGitStorage) projectHasActiveLease(projectPath string) bool {
	prefix := projectPath + string(os.PathSeparator)
	for _, l := range s.locks.ActiveLeases() {
		if l.Path == projectPath || strings.HasPrefix(l.Path, prefix) {
			return true
		}
	}
	return false
}
