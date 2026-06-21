package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/nsregistry"
	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// seedProject creates ns/proj with one committed file and waits for the commit.
func seedProject(t *testing.T, s *FSGitStorage, ns, proj, path, content string) {
	t.Helper()
	if err := s.CreateProject(ns, proj); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", ns, proj, path, content, nil); err != nil {
		t.Fatal(err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}
}

// #1 (core) — MoveProject relocates the 3 artefacts as-is, preserves git history, re-keys
// the registry, and leaves the project healthy at the new location.
func TestMoveProject_HappyPath(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "src", "proj", "a.md", "v1")
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}

	if err := s.MoveProject(ctx, "src", "proj", "dst"); err != nil {
		t.Fatalf("MoveProject: %v", err)
	}

	// On disk: the dir + both sibling DBs are at dst, none at src.
	if _, err := os.Stat(filepath.Join(s.baseDir, "dst", "proj", ".git")); err != nil {
		t.Errorf("moved project git repo must be at dst: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "src", "proj")); !os.IsNotExist(err) {
		t.Errorf("source project dir must be gone: %v", err)
	}
	if _, err := os.Stat(s.catalogPath("dst", "proj")); err != nil {
		t.Errorf("catalog must be at dst: %v", err)
	}
	if _, err := os.Stat(s.catalogPath("src", "proj")); !os.IsNotExist(err) {
		t.Errorf("source catalog must be gone: %v", err)
	}
	if _, err := os.Stat(s.indexPath("src", "proj")); !os.IsNotExist(err) {
		t.Errorf("source index must be gone: %v", err)
	}
	// git history intact at the new location.
	hist, err := s.GetHistory("dst", "proj", "a.md", 0)
	if err != nil || len(hist) == 0 {
		t.Fatalf("git history must survive the move: hist=%d err=%v", len(hist), err)
	}
	// Content readable; registry re-keyed.
	if got, _ := s.ReadFile("dst", "proj", "a.md"); got != "v1" {
		t.Errorf("moved project read = %q, want v1", got)
	}
	if has, _ := s.nsReg.HasProject("src", "proj"); has {
		t.Error("registry must not list the project at src")
	}
	if has, _ := s.nsReg.HasProject("dst", "proj"); !has {
		t.Error("registry must list the project at dst")
	}
	if s.State("dst", "proj") == StateDangerous {
		t.Error("moved project must not be dangerous")
	}
}

// #3 — GitHub-transfer rules: no overwrite, target must exist, no same-namespace move.
func TestMoveProject_TransferRules(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "src", "proj", "a.md", "x")

	// Target namespace does not exist/managed → refused.
	if err := s.MoveProject(ctx, "src", "proj", "nope"); err == nil {
		t.Error("move to a non-existent/unmanaged target must be refused")
	}
	// Same namespace → refused.
	if err := s.MoveProject(ctx, "src", "proj", "src"); err == nil {
		t.Error("move onto the same namespace must be refused")
	}
	// Collision: target already has a project of that name → refused (no overwrite).
	if err := s.CreateProject("dst", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveProject(ctx, "src", "proj", "dst"); err == nil {
		t.Error("move must be refused when the target already has a project of that name")
	}
	// The source is untouched by the refusals.
	if _, err := os.Stat(filepath.Join(s.baseDir, "src", "proj", ".git")); err != nil {
		t.Errorf("a refused move must leave the source intact: %v", err)
	}
}

// #4 — locking: a project marked moving refuses writes with the retriable ErrProjectMoving;
// a held write lease on the source blocks the move.
func TestMoveProject_Locking(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "src", "proj", "a.md", "x")
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}

	// The moving-set fences writes with the retriable error.
	s.markMoving("src", "proj")
	if err := s.checkWritable("src", "proj"); !errors.Is(err, ErrProjectMoving) {
		t.Errorf("a write to a moving project must return ErrProjectMoving, got %v", err)
	}
	s.unmarkMoving("src", "proj")
	if err := s.checkWritable("src", "proj"); errors.Is(err, ErrProjectMoving) {
		t.Error("after unmark, writes must no longer be fenced as moving")
	}

	// A held write lease on the source blocks the move.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = s.locks.WithLock(ctx, "sess", filepath.Join(s.baseDir, "src", "proj", "a.md"), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	if err := s.MoveProject(ctx, "src", "proj", "dst"); err == nil {
		t.Error("a move must be refused while a write lease is held on the source")
	}
	close(release)
}

// #5 (core) — interrupted-move AUTOMATIC recovery: a crash with the dir already at the new
// location (journal present, registry not yet swapped) is auto-completed at StartupInit; a
// crash before the rename is rolled back. No operator action.
func TestMoveProject_AutoRecovery_ForwardOnRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s1, err := NewFSGitStorageWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedProject(t, s1, "src", "proj", "a.md", "v1")
	if err := s1.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-move: journal at dir_moved, the dir already renamed to dst, but
	// the registry NOT yet swapped (src still lists it) and the .dbs still at src.
	s1.setOpJournal(nsregistry.OpJournal{
		Op: opMove, OldNamespace: "src", OldProject: "proj", NewNamespace: "dst", NewProject: "proj", Project: "proj", Phase: movePhaseDirMoved,
	})
	s1.evictProjectHandles("src", "proj")
	if rerr := os.Rename(filepath.Join(dir, "src", "proj"), filepath.Join(dir, "dst", "proj")); rerr != nil {
		t.Fatal(rerr)
	}
	if cerr := s1.Close(); cerr != nil { // release the registry lock for the "restart"
		t.Fatal(cerr)
	}

	// Restart: StartupInit auto-completes the move with no manual step.
	s2, err := NewFSGitStorageWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.StartupInit(ctx)

	if has, _ := s2.nsReg.HasProject("dst", "proj"); !has {
		t.Error("auto-recovery must finish the registry swap to dst")
	}
	if has, _ := s2.nsReg.HasProject("src", "proj"); has {
		t.Error("auto-recovery must remove the project from src")
	}
	if _, found, _ := s2.nsReg.GetOpJournal(); found {
		t.Error("auto-recovery must clear the move journal")
	}
	if _, err := os.Stat(s2.catalogPath("dst", "proj")); err != nil {
		t.Errorf("auto-recovery must relocate the catalog to dst: %v", err)
	}
}

func TestMoveProject_AutoRecovery_RollbackBeforeRename(t *testing.T) {
	s := newEmptyStorage(t)
	seedProject(t, s, "src", "proj", "a.md", "v1")
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash BEFORE the rename: journal at started, dir still at src.
	s.setOpJournal(nsregistry.OpJournal{
		Op: opMove, OldNamespace: "src", OldProject: "proj", NewNamespace: "dst", NewProject: "proj", Project: "proj", Phase: movePhaseStarted,
	})

	s.recoverInterruptedOp()

	if _, found, _ := s.nsReg.GetOpJournal(); found {
		t.Error("rollback must clear the journal")
	}
	if has, _ := s.nsReg.HasProject("src", "proj"); !has {
		t.Error("rollback must keep the project at src")
	}
	if has, _ := s.nsReg.HasProject("dst", "proj"); has {
		t.Error("rollback must not register the project at dst")
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "src", "proj", ".git")); err != nil {
		t.Errorf("rollback must leave the source dir in place: %v", err)
	}
}

// #2 — the grant cascade-REWRITE on move: project-specific grants follow the project;
// namespace-wide and wildcard grants are untouched; across user + oauth + invite scopes.
func TestMoveProject_GrantRewrite(t *testing.T) {
	s, us, oas := storageWithStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := us.CreateUser(&userstore.UserRecord{Email: "proj-user@x", Scope: "namespace:src/proj:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "ns-user@x", Scope: "namespace:src:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "super@x", Scope: "*:admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := oas.NewSeries("c1", oauthstore.Principal{Name: "n"}, "res", "namespace:src/proj:r", now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	seedProject(t, s, "src", "proj", "a.md", "x")
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveProject(ctx, "src", "proj", "dst"); err != nil {
		t.Fatalf("MoveProject: %v", err)
	}

	if got := userScope(t, us, "proj-user@x"); got != "namespace:dst/proj:rw" {
		t.Errorf("project grant must follow the move: got %q, want namespace:dst/proj:rw", got)
	}
	if got := userScope(t, us, "ns-user@x"); got != "namespace:src:admin" {
		t.Errorf("namespace-wide grant must be untouched: got %q", got)
	}
	if got := userScope(t, us, "super@x"); got != "*:admin" {
		t.Errorf("wildcard grant must be untouched: got %q", got)
	}
	series, _ := oas.List()
	for _, se := range series {
		if se.ClientID == "c1" && se.Scope != "namespace:dst/proj:r" {
			t.Errorf("oauth project grant must follow the move: got %q", se.Scope)
		}
	}
}
