package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage/nsregistry"
)

// Project move between namespaces (B-28). A move is a SPECIAL op with a coarse-lock
// licence: it is serialized (moveMu), fences the source against in-flight writes (the
// part-1 lease fence + the moving-set), and journals its intent so an interruption is
// AUTOMATICALLY resumed or rolled back at startup — a manual move never also needs manual
// health recovery. The catalog/index .db's are namespace-informational only (the
// investigation: VerifyInvariant is path-relative), so the three artefacts RELOCATE AS-IS
// via os.Rename — no rebuild; git history travels with the self-contained project dir; both
// namespaces are under base_dir (same fs ⇒ atomic rename).

// Op-journal phases (the journal also records the op + old/new ns/proj). Recovery is driven
// primarily by ON-DISK reality (where the dir actually is); the phase is a logged hint.
const (
	movePhaseStarted  = "started"   // journal written; the dir rename has not completed
	movePhaseDirMoved = "dir_moved" // the dir rename completed; the registry/grant swap may not have
)

// Op-journal op kinds (B-28 ns/proj rename generalised the move-journal into one op-journal).
// A legacy move-journal decodes with Op=="" — recovery treats that as opMove.
const (
	opMove            = "move"
	opRenameProject   = "rename_project"
	opRenameNamespace = "rename_namespace"
)

func (s *FSGitStorage) markMoving(namespace, projectName string) {
	s.movingMu.Lock()
	s.moving[projectKey(namespace, projectName)] = true
	s.movingMu.Unlock()
}

func (s *FSGitStorage) unmarkMoving(namespace, projectName string) {
	s.movingMu.Lock()
	delete(s.moving, projectKey(namespace, projectName))
	s.movingMu.Unlock()
}

func (s *FSGitStorage) isMoving(namespace, projectName string) bool {
	s.movingMu.Lock()
	defer s.movingMu.Unlock()
	return s.moving[projectKey(namespace, projectName)]
}

// MoveProject relocates an entire project from oldNs to newNs (B-28 project move). It is a
// special, serialized, journaled op (see the package doc). GitHub-transfer rules: the target
// namespace must pre-exist and be managed; it refuses if the target already has a project of
// that name (no silent overwrite), and refuses a same-namespace / self move. The project
// name is preserved (a move is not a rename). super-user-gated at the call sites.
func (s *FSGitStorage) MoveProject(ctx context.Context, oldNs, projectName, newNs string) error {
	if oldNs == "" {
		oldNs = DefaultNamespace
	}
	if newNs == "" {
		newNs = DefaultNamespace
	}
	oldDir, err := s.getProjectPath(oldNs, projectName) // validates names
	if err != nil {
		return err
	}
	newDir, err := s.getProjectPath(newNs, projectName)
	if err != nil {
		return err
	}
	if oldNs == newNs {
		return fmt.Errorf("cannot move project %s/%s onto the same namespace", oldNs, projectName)
	}
	if !hasGitRepo(oldDir) {
		return ErrProjectNotFound
	}
	// GitHub-transfer rules: target namespace must pre-exist + be managed; no overwrite.
	if s.nsReg != nil {
		if _, found, gerr := s.nsReg.Get(newNs); gerr != nil {
			return fmt.Errorf("move: read target namespace: %w", gerr)
		} else if !found {
			return fmt.Errorf("target namespace %q does not exist (create it first)", newNs)
		}
		if has, herr := s.nsReg.HasProject(newNs, projectName); herr != nil {
			return fmt.Errorf("move: collision check: %w", herr)
		} else if has {
			return fmt.Errorf("a project named %q already exists in namespace %q", projectName, newNs)
		}
	}
	// Belt-and-suspenders: the target dir must not already exist on disk.
	if _, statErr := os.Stat(newDir); statErr == nil {
		return fmt.Errorf("a directory for %s/%s already exists on disk", newNs, projectName)
	}

	// Special-op coarse lock: serialize all moves, then fence the source.
	s.moveMu.Lock()
	defer s.moveMu.Unlock()
	if s.projectHasActiveLease(oldDir) {
		return fmt.Errorf("cannot move project %s/%s: a write is in progress", oldNs, projectName)
	}
	s.markMoving(oldNs, projectName)
	s.markMoving(newNs, projectName)
	defer s.unmarkMoving(oldNs, projectName)
	defer s.unmarkMoving(newNs, projectName)

	// Journal the intent so an interruption is auto-recovered at startup.
	s.setOpJournal(nsregistry.OpJournal{
		Op: opMove, OldNamespace: oldNs, OldProject: projectName,
		NewNamespace: newNs, NewProject: projectName, Project: projectName, Phase: movePhaseStarted,
	})

	// Evict the in-memory handles so the .db siblings can be renamed with no open handle.
	s.evictProjectHandles(oldNs, projectName)

	// The atomic pivot: rename the project dir (git history travels). On failure nothing
	// has moved — clear the journal and abort.
	if rerr := os.Rename(oldDir, newDir); rerr != nil {
		s.clearOpJournal()
		return fmt.Errorf("move: rename project dir %s/%s → %s/%s: %w", oldNs, projectName, newNs, projectName, rerr)
	}
	s.setOpJournal(nsregistry.OpJournal{
		Op: opMove, OldNamespace: oldNs, OldProject: projectName,
		NewNamespace: newNs, NewProject: projectName, Project: projectName, Phase: movePhaseDirMoved,
	})

	// Complete the (idempotent) remainder: relocate the .db siblings, swap the registry,
	// rewrite grants, emit the event. On failure the journal stays so StartupInit finishes
	// it forward automatically.
	if cerr := s.completeMoveAfterRename(ctx, oldNs, projectName, newNs); cerr != nil {
		return fmt.Errorf("move: complete %s/%s → %s/%s: %w", oldNs, projectName, newNs, projectName, cerr)
	}
	s.clearOpJournal()
	return nil
}

// completeMoveAfterRename runs the post-dir-rename steps, all IDEMPOTENT so they can be
// re-run by the startup recovery: relocate the catalog/index siblings, swap the registry
// (one atomic tx), grant cascade-REWRITE, emit project_moved. The catalog/index are
// disposable derivatives, so a failed sibling rename falls back to remove-old + lazy
// rebuild at the new location.
func (s *FSGitStorage) completeMoveAfterRename(ctx context.Context, oldNs, projectName, newNs string) error {
	s.relocateSiblingDB(s.catalogPath(oldNs, projectName), s.catalogPath(newNs, projectName))
	s.relocateSiblingDB(s.indexPath(oldNs, projectName), s.indexPath(newNs, projectName))

	if s.nsReg != nil {
		if err := s.nsReg.MoveProject(oldNs, projectName, newNs); err != nil {
			return fmt.Errorf("registry move: %w", err)
		}
	}
	if s.scopeCleaner != nil {
		if err := s.scopeCleaner.RewriteProject(oldNs, projectName, newNs, projectName); err != nil {
			return fmt.Errorf("grant rewrite: %w", err)
		}
	}

	// project_moved change event (decision 6) + a notify so /ws/ui clients refetch. The
	// new in-memory handles open lazily at the new key on next access.
	s.notify.NotifyFrom(notify.SenderFrom(ctx), "project.move", newNs+"/"+projectName, "")
	s.emit(ChangeEvent{
		Event:        "project_moved",
		Namespace:    newNs,
		Project:      projectName,
		OldNamespace: oldNs,
		OldProject:   projectName,
		Timestamp:    time.Now(),
	})
	return nil
}

// relocateSiblingDB renames a catalog/index .db from old→new. The .db's are disposable
// derivatives, so on any rename failure the stale old file is removed and a fresh one is
// rebuilt lazily at the new location (catalogFor/indexFor on next access, plus the repair
// sweep) — never a hard failure. A missing source is a no-op.
func (s *FSGitStorage) relocateSiblingDB(oldPath, newPath string) {
	if _, err := os.Stat(oldPath); err != nil {
		return // nothing to relocate
	}
	if rerr := os.Rename(oldPath, newPath); rerr != nil {
		s.log().Warn("move: sibling db relocate failed; removing stale source for lazy rebuild",
			"old", oldPath, "new", newPath, "err", rerr)
		if remErr := os.Remove(oldPath); remErr != nil && !os.IsNotExist(remErr) {
			s.log().Warn("move: stale sibling db remove failed", "path", oldPath, "err", remErr)
		}
	}
}

// --- op journal helpers (best-effort; failures are logged, never fatal) ---

func (s *FSGitStorage) setOpJournal(j nsregistry.OpJournal) {
	if s.nsReg == nil {
		return
	}
	if err := s.nsReg.SetOpJournal(j); err != nil {
		s.log().Warn("op: write journal failed", "err", err)
	}
}

func (s *FSGitStorage) clearOpJournal() {
	if s.nsReg == nil {
		return
	}
	if err := s.nsReg.ClearOpJournal(); err != nil {
		s.log().Warn("op: clear journal failed", "err", err)
	}
}

// recoverInterruptedOp is called at StartupInit BEFORE discovery/rescue: if a SPECIAL op
// (move / rename_project / rename_namespace) was in progress (a journal entry survives), it
// AUTOMATICALLY resumes or rolls back to a consistent state with NO operator action. Recovery
// is driven by ON-DISK reality (where the dir actually is), since os.Rename is atomic but the
// phase marker may lag a crash. A legacy move-journal decodes with Op=="" → treated as a move.
//
//   - move / rename_project (the dir is a project dir): probe hasGitRepo at the new vs old
//     project path. New side has the repo → finish forward (idempotent); old side → roll back.
//   - rename_namespace (the dir is the whole namespace dir, moved atomically): probe directory
//     existence at <base>/<new> vs <base>/<old>. New exists → finish forward; old → roll back.
//   - neither/both → inconsistent: log and clear, leaving stage-B health as the last resort.
func (s *FSGitStorage) recoverInterruptedOp() {
	if s.nsReg == nil {
		return
	}
	j, found, err := s.nsReg.GetOpJournal()
	if err != nil {
		s.log().Error("op recovery: read journal failed", "err", err)
		return
	}
	if !found {
		return
	}
	op := j.Op
	if op == "" {
		op = opMove // legacy move-journal
	}

	switch op {
	case opRenameNamespace:
		if !s.recoverNamespaceRename(j) {
			return // forward-complete failed; keep the journal so a later restart retries
		}
	default: // opMove, opRenameProject — both relocate a project dir
		oldProj := j.OldProject
		if oldProj == "" {
			oldProj = j.Project
		}
		newProj := j.NewProject
		if newProj == "" {
			newProj = j.Project
		}
		oldDir := filepath.Join(s.baseDir, j.OldNamespace, oldProj)
		newDir := filepath.Join(s.baseDir, j.NewNamespace, newProj)
		newHasGit := hasGitRepo(newDir)
		oldHasGit := hasGitRepo(oldDir)
		oldKey := j.OldNamespace + "/" + oldProj
		newKey := j.NewNamespace + "/" + newProj
		switch {
		case newHasGit && !oldHasGit:
			var cerr error
			if op == opRenameProject {
				cerr = s.completeRenameProject(context.Background(), j.OldNamespace, oldProj, newProj)
			} else {
				cerr = s.completeMoveAfterRename(context.Background(), j.OldNamespace, oldProj, j.NewNamespace)
			}
			if cerr != nil {
				s.log().Error("op recovery: forward-complete failed; left for health",
					"op", op, "old", oldKey, "new", newKey, "err", cerr)
				return // keep the journal so a later restart retries
			}
			s.log().Info("op recovery: interrupted op auto-completed", "op", op, "old", oldKey, "new", newKey, "phase", j.Phase)
		case oldHasGit && !newHasGit:
			s.rollbackProjectDirOp(op, j.OldNamespace, oldProj, j.NewNamespace, newProj)
			s.log().Info("op recovery: interrupted op auto-rolled-back to source", "op", op, "old", oldKey, "new", newKey, "phase", j.Phase)
		default:
			s.log().Error("op recovery: inconsistent on-disk state; clearing journal, leaving to health",
				"op", op, "old", oldKey, "new", newKey, "oldHasGit", oldHasGit, "newHasGit", newHasGit)
		}
	}
	s.clearOpJournal()
}

// rollbackProjectDirOp restores the managed record/grants to the source after a pre-rename
// interruption of a move or project-rename, and removes any stray target-side .db siblings.
// Best-effort; logged. (The registry swap + grant rewrite run AFTER the dir rename, so a
// pre-rename crash means they had not run — these calls are defensive no-ops in the common case.)
func (s *FSGitStorage) rollbackProjectDirOp(op, oldNs, oldProj, newNs, newProj string) {
	if s.nsReg != nil {
		if op == opRenameProject {
			if has, _ := s.nsReg.HasProject(oldNs, newProj); has {
				if err := s.nsReg.RenameProject(oldNs, newProj, oldProj); err != nil {
					s.log().Warn("rename rollback: registry restore failed", "err", err)
				}
			}
		} else { // move
			if has, _ := s.nsReg.HasProject(newNs, newProj); has {
				if err := s.nsReg.MoveProject(newNs, newProj, oldNs); err != nil {
					s.log().Warn("move rollback: registry restore failed", "err", err)
				}
			} else if err := s.nsReg.AddProject(oldNs, oldProj); err != nil {
				s.log().Warn("move rollback: re-add to source failed", "err", err)
			}
		}
	}
	if s.scopeCleaner != nil {
		// No-op if no grants were rewritten (they are rewritten only after the rename).
		if op == opRenameProject {
			_ = s.scopeCleaner.RewriteProject(oldNs, newProj, oldNs, oldProj)
		} else {
			_ = s.scopeCleaner.RewriteProject(newNs, newProj, oldNs, oldProj)
		}
	}
	for _, p := range []string{s.catalogPath(newNs, newProj), s.indexPath(newNs, newProj)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			s.log().Warn("op rollback: remove stray target db failed", "path", p, "err", err)
		}
	}
}
