package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage/nsregistry"
	"github.com/sopranoworks/shoka/internal/utils"
)

// Namespace/project RENAME (B-28). Two SPECIAL ops under the move coarse-lock licence,
// REUSING and GENERALISING the move machinery: the op-mutex (moveMu) + the moving-set fence
// + the lease fence, the relocate-as-is .db handling, the atomic registry re-key, the grant
// cascade-REWRITE, and the on-disk-reality op-journal recovery (see projectmove.go).
//
//   - RenameProject(ns, old, new) — within ONE namespace: identical to a move with the
//     namespace fixed and the project NAME changing. The dir + 2 sibling .db's relocate
//     as-is; the registry re-keys the name within the record; the project-specific grant is
//     rewritten namespace:<ns>/<old> → namespace:<ns>/<new>. admin-on-ns (gated at the surfaces).
//   - RenameNamespace(old, new) — the WIDER op: a SINGLE whole-dir os.Rename relabels the
//     namespace, carrying every project dir + sibling .db at once (the .db's live inside the
//     namespace dir, _meta is informational, paths are computed at call time). The registry
//     re-keys the namespace record; a DUAL grant rewrite re-homes BOTH namespace-wide AND every
//     project-specific grant. It quiesces ALL its projects (movingNs) + lease-fences each.
//     super-user (gated at the surfaces). `default` is rename-protected.

// --- namespace-rename quiesce set (a whole-namespace fence checkWritable consults) ---

func (s *FSGitStorage) markMovingNs(namespace string) {
	s.movingMu.Lock()
	s.movingNs[namespace] = true
	s.movingMu.Unlock()
}

func (s *FSGitStorage) unmarkMovingNs(namespace string) {
	s.movingMu.Lock()
	delete(s.movingNs, namespace)
	s.movingMu.Unlock()
}

func (s *FSGitStorage) isMovingNs(namespace string) bool {
	s.movingMu.Lock()
	defer s.movingMu.Unlock()
	return s.movingNs[namespace]
}

// namespaceDirExists reports whether path is an existing directory (the namespace-rename recovery
// probe: the whole namespace dir moves atomically, so it is wholly at one side).
func namespaceDirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// RenameProject renames a project WITHIN its namespace (B-28 ns/proj rename). It mirrors
// MoveProject with the namespace fixed: it validates (names, old≠new, source has a git repo,
// no target-name collision, target dir absent), then under the op-mutex fences the source,
// journals, evicts handles, renames the dir (the atomic pivot, git travels), and completes the
// idempotent remainder (relocate the .db siblings, re-key the registry, rewrite the
// project-specific grant, emit project_renamed). admin-on-ns-gated at the call sites.
func (s *FSGitStorage) RenameProject(ctx context.Context, namespace, oldName, newName string) error {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	oldDir, err := s.getProjectPath(namespace, oldName) // validates names
	if err != nil {
		return err
	}
	newDir, err := s.getProjectPath(namespace, newName) // validates names
	if err != nil {
		return err
	}
	if oldName == newName {
		return fmt.Errorf("cannot rename project %s/%s onto the same name", namespace, oldName)
	}
	if !hasGitRepo(oldDir) {
		return ErrProjectNotFound
	}
	// No silent overwrite: the target name must be free (registry + on-disk).
	if s.nsReg != nil {
		if has, herr := s.nsReg.HasProject(namespace, newName); herr != nil {
			return fmt.Errorf("rename: collision check: %w", herr)
		} else if has {
			return fmt.Errorf("a project named %q already exists in namespace %q", newName, namespace)
		}
	}
	if _, statErr := os.Stat(newDir); statErr == nil {
		return fmt.Errorf("a directory for %s/%s already exists on disk", namespace, newName)
	}

	// Special-op coarse lock: serialize all special ops, then fence the source.
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.projectHasActiveLease(oldDir) {
		return fmt.Errorf("cannot rename project %s/%s: a write is in progress", namespace, oldName)
	}
	s.markMoving(namespace, oldName)
	s.markMoving(namespace, newName)
	defer s.unmarkMoving(namespace, oldName)
	defer s.unmarkMoving(namespace, newName)

	s.setOpJournal(nsregistry.OpJournal{
		Op: opRenameProject, OldNamespace: namespace, OldProject: oldName,
		NewNamespace: namespace, NewProject: newName, Phase: movePhaseStarted,
	})

	s.evictProjectHandles(namespace, oldName)

	if rerr := os.Rename(oldDir, newDir); rerr != nil {
		s.clearOpJournal()
		return fmt.Errorf("rename: rename project dir %s/%s → %s/%s: %w", namespace, oldName, namespace, newName, rerr)
	}
	s.setOpJournal(nsregistry.OpJournal{
		Op: opRenameProject, OldNamespace: namespace, OldProject: oldName,
		NewNamespace: namespace, NewProject: newName, Phase: movePhaseDirMoved,
	})

	if cerr := s.completeRenameProject(ctx, namespace, oldName, newName); cerr != nil {
		return fmt.Errorf("rename: complete %s/%s → %s/%s: %w", namespace, oldName, namespace, newName, cerr)
	}
	s.clearOpJournal()
	return nil
}

// completeRenameProject runs the post-dir-rename steps, all IDEMPOTENT so the live tail and
// the startup recovery share them: relocate the catalog/index siblings, re-key the registry,
// rewrite the project-specific grant, emit project_renamed.
func (s *FSGitStorage) completeRenameProject(ctx context.Context, namespace, oldName, newName string) error {
	s.relocateSiblingDB(s.catalogPath(namespace, oldName), s.catalogPath(namespace, newName))
	s.relocateSiblingDB(s.indexPath(namespace, oldName), s.indexPath(namespace, newName))

	if s.nsReg != nil {
		if err := s.nsReg.RenameProject(namespace, oldName, newName); err != nil {
			return fmt.Errorf("registry rename: %w", err)
		}
	}
	if s.scopeCleaner != nil {
		if err := s.scopeCleaner.RewriteProject(namespace, oldName, namespace, newName); err != nil {
			return fmt.Errorf("grant rewrite: %w", err)
		}
	}

	s.notify.NotifyFrom(notify.SenderFrom(ctx), "project.rename", namespace+"/"+newName, "")
	s.emit(ChangeEvent{
		Event:        "project_renamed",
		Namespace:    namespace,
		Project:      newName,
		OldNamespace: namespace,
		OldProject:   oldName,
		Timestamp:    time.Now(),
	})
	return nil
}

// RenameNamespace relabels a whole managed namespace old→new (B-28 ns/proj rename) — the WIDER
// op. It validates (names, old≠new, old not the protected `default`, no target collision,
// target dir absent), then under the op-mutex lease-fences EVERY project under the namespace,
// quiesces the namespace (movingNs), journals, evicts every project's handles, renames the
// WHOLE namespace dir in one atomic os.Rename (carrying every project + sibling .db), and
// completes the idempotent remainder (re-key the registry, the DUAL grant rewrite, emit
// namespace_renamed). Non-empty namespaces rename fine (a relabel, not the empty-only delete).
// super-user-gated at the call sites.
func (s *FSGitStorage) RenameNamespace(ctx context.Context, oldName, newName string) error {
	if oldName == "" {
		oldName = DefaultNamespace
	}
	if !utils.IsValidName(oldName) {
		return fmt.Errorf("invalid namespace: %s", oldName)
	}
	if !utils.IsValidName(newName) {
		return fmt.Errorf("invalid namespace: %s", newName)
	}
	if oldName == newName {
		return fmt.Errorf("cannot rename namespace %s onto the same name", oldName)
	}
	// `default` is rename-protected (it is the namespace-omitted entry point): refuse renaming
	// it away, and refuse renaming any namespace TO it (it always exists ⇒ a collision).
	if oldName == DefaultNamespace {
		return fmt.Errorf("the %q namespace cannot be renamed (it is the default entry point)", DefaultNamespace)
	}
	if newName == DefaultNamespace {
		return fmt.Errorf("cannot rename a namespace to %q (it already exists)", DefaultNamespace)
	}
	if s.nsReg != nil {
		if managed, herr := s.nsReg.HasNamespace(oldName); herr != nil {
			return fmt.Errorf("rename: read source namespace: %w", herr)
		} else if !managed {
			return fmt.Errorf("namespace %q is not managed", oldName)
		}
		if has, herr := s.nsReg.HasNamespace(newName); herr != nil {
			return fmt.Errorf("rename: collision check: %w", herr)
		} else if has {
			return fmt.Errorf("a namespace named %q already exists", newName)
		}
	}
	oldDir := filepath.Join(s.baseDir, oldName)
	newDir := filepath.Join(s.baseDir, newName)
	// Belt-and-suspenders: the target dir must not already exist on disk (catches an unmanaged
	// on-disk namespace dir colliding with the new name).
	if _, statErr := os.Stat(newDir); statErr == nil {
		return fmt.Errorf("a directory for namespace %q already exists on disk", newName)
	}

	// Special-op coarse lock: serialize all special ops, then quiesce the WHOLE namespace.
	s.moveMu.Lock()
	defer s.moveMu.Unlock()

	projects, err := s.ListProjects(oldName)
	if err != nil {
		return fmt.Errorf("rename: list projects of %q: %w", oldName, err)
	}
	for _, p := range projects {
		if s.projectHasActiveLease(filepath.Join(oldDir, p)) {
			return fmt.Errorf("cannot rename namespace %s: a write is in progress on project %s", oldName, p)
		}
	}
	s.markMovingNs(oldName)
	defer s.unmarkMovingNs(oldName)

	s.setOpJournal(nsregistry.OpJournal{
		Op: opRenameNamespace, OldNamespace: oldName, NewNamespace: newName, Phase: movePhaseStarted,
	})

	// Evict every project's in-memory handles so the .db siblings travel with the dir cleanly.
	for _, p := range projects {
		s.evictProjectHandles(oldName, p)
	}

	// The atomic pivot: one whole-dir rename moves every project + sibling .db at once.
	if rerr := os.Rename(oldDir, newDir); rerr != nil {
		s.clearOpJournal()
		return fmt.Errorf("rename: rename namespace dir %s → %s: %w", oldName, newName, rerr)
	}
	s.setOpJournal(nsregistry.OpJournal{
		Op: opRenameNamespace, OldNamespace: oldName, NewNamespace: newName, Phase: movePhaseDirMoved,
	})

	if cerr := s.completeRenameNamespace(ctx, oldName, newName); cerr != nil {
		return fmt.Errorf("rename: complete namespace %s → %s: %w", oldName, newName, cerr)
	}
	s.clearOpJournal()
	return nil
}

// completeRenameNamespace runs the post-dir-rename steps, all IDEMPOTENT so the live tail and
// the startup recovery share them: re-key the registry namespace record, the DUAL grant
// rewrite (namespace-wide AND every project-specific grant), emit namespace_renamed.
func (s *FSGitStorage) completeRenameNamespace(ctx context.Context, oldName, newName string) error {
	if s.nsReg != nil {
		if err := s.nsReg.RenameNamespace(oldName, newName); err != nil {
			return fmt.Errorf("registry rename: %w", err)
		}
	}
	if s.scopeCleaner != nil {
		if err := s.scopeCleaner.RewriteNamespace(oldName, newName); err != nil {
			return fmt.Errorf("grant rewrite: %w", err)
		}
	}

	s.notify.NotifyFrom(notify.SenderFrom(ctx), "namespace.rename", newName, "")
	s.emit(ChangeEvent{
		Event:        "namespace_renamed",
		Namespace:    newName,
		OldNamespace: oldName,
		Timestamp:    time.Now(),
	})
	return nil
}

// recoverNamespaceRename auto-resolves an interrupted namespace rename from on-disk reality
// (the whole namespace dir moved atomically). It returns true when the op-journal may be
// cleared (resolved or left to health) and false to KEEP the journal for a later retry.
func (s *FSGitStorage) recoverNamespaceRename(j nsregistry.OpJournal) bool {
	oldDir := filepath.Join(s.baseDir, j.OldNamespace)
	newDir := filepath.Join(s.baseDir, j.NewNamespace)
	newExists := namespaceDirExists(newDir)
	oldExists := namespaceDirExists(oldDir)
	switch {
	case newExists && !oldExists:
		if cerr := s.completeRenameNamespace(context.Background(), j.OldNamespace, j.NewNamespace); cerr != nil {
			s.log().Error("op recovery: namespace-rename forward-complete failed; left for health",
				"old", j.OldNamespace, "new", j.NewNamespace, "err", cerr)
			return false
		}
		s.log().Info("op recovery: interrupted namespace rename auto-completed",
			"old", j.OldNamespace, "new", j.NewNamespace, "phase", j.Phase)
	case oldExists && !newExists:
		s.rollbackNamespaceRename(j.OldNamespace, j.NewNamespace)
		s.log().Info("op recovery: interrupted namespace rename auto-rolled-back to source",
			"old", j.OldNamespace, "new", j.NewNamespace, "phase", j.Phase)
	default:
		s.log().Error("op recovery: inconsistent namespace-rename on-disk state; clearing journal, leaving to health",
			"old", j.OldNamespace, "new", j.NewNamespace, "oldExists", oldExists, "newExists", newExists)
	}
	return true
}

// rollbackNamespaceRename restores the managed record/grants to the source after a pre-rename
// interruption of a namespace rename. Best-effort; logged. (The registry re-key + grant
// rewrite run AFTER the dir rename, so a pre-rename crash means they had not run — these are
// defensive no-ops in the common case.)
func (s *FSGitStorage) rollbackNamespaceRename(oldName, newName string) {
	if s.nsReg != nil {
		oldManaged, _ := s.nsReg.HasNamespace(oldName)
		newManaged, _ := s.nsReg.HasNamespace(newName)
		if newManaged && !oldManaged {
			if err := s.nsReg.RenameNamespace(newName, oldName); err != nil {
				s.log().Warn("namespace-rename rollback: registry restore failed", "err", err)
			}
		}
	}
	if s.scopeCleaner != nil {
		_ = s.scopeCleaner.RewriteNamespace(newName, oldName)
	}
}
