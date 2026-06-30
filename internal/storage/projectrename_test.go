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

// #1 (core) — RenameProject relocates the 3 artefacts as-is within the namespace, preserves
// git history, re-keys the registry to the new name, and leaves the project healthy.
func TestRenameProject_HappyPath(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "ns1", "old", "a.md", "v1")

	if err := s.RenameProject(ctx, "ns1", "old", "new"); err != nil {
		t.Fatalf("RenameProject: %v", err)
	}

	// On disk: dir + both sibling DBs at ns1/new, none at ns1/old.
	if _, err := os.Stat(filepath.Join(s.baseDir, "ns1", "new", ".git")); err != nil {
		t.Errorf("renamed project git repo must be at ns1/new: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "ns1", "old")); !os.IsNotExist(err) {
		t.Errorf("old project dir must be gone: %v", err)
	}
	if _, err := os.Stat(s.catalogPath("ns1", "new")); err != nil {
		t.Errorf("catalog must be at ns1/new: %v", err)
	}
	if _, err := os.Stat(s.catalogPath("ns1", "old")); !os.IsNotExist(err) {
		t.Errorf("old catalog must be gone: %v", err)
	}
	if _, err := os.Stat(s.indexPath("ns1", "old")); !os.IsNotExist(err) {
		t.Errorf("old index must be gone: %v", err)
	}
	// git history intact + content readable.
	if hist, err := s.GetHistory("ns1", "new", "a.md", 0); err != nil || len(hist) == 0 {
		t.Fatalf("git history must survive the rename: hist=%d err=%v", len(hist), err)
	}
	if got, _ := s.ReadFile("ns1", "new", "a.md"); got != "v1" {
		t.Errorf("renamed project read = %q, want v1", got)
	}
	// Registry re-keyed within the namespace.
	if has, _ := s.nsReg.HasProject("ns1", "old"); has {
		t.Error("registry must not list the old project name")
	}
	if has, _ := s.nsReg.HasProject("ns1", "new"); !has {
		t.Error("registry must list the new project name")
	}
	if s.State("ns1", "new") == StateDangerous {
		t.Error("renamed project must not be dangerous")
	}
}

// #2 (core) — RenameNamespace relabels a NON-EMPTY namespace (≥2 projects) in one whole-dir
// move: every project dir + sibling DB now lives under the new namespace; the registry
// namespace record is re-keyed carrying its projects; git history of each is intact.
func TestRenameNamespace_HappyPath(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "src", "p1", "a.md", "one")
	seedProject(t, s, "src", "p2", "b.md", "two")

	if err := s.RenameNamespace(ctx, "src", "dst"); err != nil {
		t.Fatalf("RenameNamespace: %v", err)
	}

	// The whole namespace dir moved; the old one is gone.
	if _, err := os.Stat(filepath.Join(s.baseDir, "src")); !os.IsNotExist(err) {
		t.Errorf("old namespace dir must be gone: %v", err)
	}
	for _, p := range []string{"p1", "p2"} {
		if _, err := os.Stat(filepath.Join(s.baseDir, "dst", p, ".git")); err != nil {
			t.Errorf("project %s git repo must be at dst: %v", p, err)
		}
		if _, err := os.Stat(s.catalogPath("dst", p)); err != nil {
			t.Errorf("catalog for %s must be at dst: %v", p, err)
		}
	}
	if got, _ := s.ReadFile("dst", "p1", "a.md"); got != "one" {
		t.Errorf("dst/p1 read = %q, want one", got)
	}
	if hist, err := s.GetHistory("dst", "p2", "b.md", 0); err != nil || len(hist) == 0 {
		t.Fatalf("git history of dst/p2 must survive: hist=%d err=%v", len(hist), err)
	}
	// Registry namespace re-keyed, carrying both projects.
	if has, _ := s.nsReg.HasNamespace("src"); has {
		t.Error("registry must not list the old namespace")
	}
	if has, _ := s.nsReg.HasNamespace("dst"); !has {
		t.Error("registry must list the new namespace")
	}
	for _, p := range []string{"p1", "p2"} {
		if has, _ := s.nsReg.HasProject("dst", p); !has {
			t.Errorf("registry must carry project %s to dst", p)
		}
	}
}

// #3 — the DUAL grant cascade-REWRITE on namespace rename: BOTH namespace-wide AND every
// project-specific grant naming the old namespace follow it; wildcard + other-namespace grants
// are untouched; across user + oauth + invite scopes.
func TestRenameNamespace_DualGrantRewrite(t *testing.T) {
	s, us, oas := storageWithStores(t)
	ctx := context.Background()
	now := time.Now()

	if err := us.CreateUser(&userstore.UserRecord{Email: "nswide@x", Scope: "namespace:src:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "projadmin@x", Scope: "namespace:src/p1:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "super@x", Scope: "*:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "other@x", Scope: "namespace:other:rw"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := us.CreateInvite("invitee@x", "namespace:src/p2:rw", "super@x", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := oas.NewSeries("c1", oauthstore.Principal{Name: "n"}, "res", "namespace:src/p1:r", "", now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	seedProject(t, s, "src", "p1", "a.md", "x")
	seedProject(t, s, "src", "p2", "b.md", "y")
	if err := s.RenameNamespace(ctx, "src", "dst"); err != nil {
		t.Fatalf("RenameNamespace: %v", err)
	}

	if got := userScope(t, us, "nswide@x"); got != "namespace:dst:rw" {
		t.Errorf("namespace-wide grant must follow the rename: got %q, want namespace:dst:rw", got)
	}
	if got := userScope(t, us, "projadmin@x"); got != "namespace:dst/p1:admin" {
		t.Errorf("project-specific grant must follow the rename: got %q, want namespace:dst/p1:admin", got)
	}
	if got := userScope(t, us, "super@x"); got != "*:admin" {
		t.Errorf("wildcard grant must be untouched: got %q", got)
	}
	if got := userScope(t, us, "other@x"); got != "namespace:other:rw" {
		t.Errorf("other-namespace grant must be untouched: got %q", got)
	}
	invites, _ := us.ListInvites()
	for _, iv := range invites {
		if iv.Email == "invitee@x" && iv.Scope != "namespace:dst/p2:rw" {
			t.Errorf("invite grant must follow the rename: got %q, want namespace:dst/p2:rw", iv.Scope)
		}
	}
	series, _ := oas.List()
	for _, se := range series {
		if se.ClientID == "c1" && se.Scope != "namespace:dst/p1:r" {
			t.Errorf("oauth grant must follow the rename: got %q, want namespace:dst/p1:r", se.Scope)
		}
	}
}

// #4 — the project-rename grant rewrite: the project-specific grant follows the new name; the
// namespace-wide grant is untouched.
func TestRenameProject_GrantRewrite(t *testing.T) {
	s, us, _ := storageWithStores(t)
	ctx := context.Background()

	if err := us.CreateUser(&userstore.UserRecord{Email: "proj@x", Scope: "namespace:ns1/old:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "nsadmin@x", Scope: "namespace:ns1:admin"}); err != nil {
		t.Fatal(err)
	}

	seedProject(t, s, "ns1", "old", "a.md", "x")
	if err := s.RenameProject(ctx, "ns1", "old", "new"); err != nil {
		t.Fatalf("RenameProject: %v", err)
	}

	if got := userScope(t, us, "proj@x"); got != "namespace:ns1/new:rw" {
		t.Errorf("project grant must follow the rename: got %q, want namespace:ns1/new:rw", got)
	}
	if got := userScope(t, us, "nsadmin@x"); got != "namespace:ns1:admin" {
		t.Errorf("namespace-wide grant must be untouched: got %q", got)
	}
}

// #5 — collision + `default` protection + non-empty-allowed + validity.
func TestRename_CollisionProtectionValidity(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	seedProject(t, s, "src", "old", "a.md", "x")
	seedProject(t, s, "src", "taken", "b.md", "y")
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}

	// RenameProject refused on a target-name collision.
	if err := s.RenameProject(ctx, "src", "old", "taken"); err == nil {
		t.Error("RenameProject must be refused when the new name already exists in the namespace")
	}
	// RenameProject old==new refused; invalid new name refused.
	if err := s.RenameProject(ctx, "src", "old", "old"); err == nil {
		t.Error("RenameProject onto the same name must be refused")
	}
	if err := s.RenameProject(ctx, "src", "old", "bad/name"); err == nil {
		t.Error("RenameProject to an invalid name must be refused")
	}

	// RenameNamespace refused on a managed-name collision.
	if err := s.RenameNamespace(ctx, "src", "dst"); err == nil {
		t.Error("RenameNamespace must be refused when the new namespace already exists")
	}
	// RenameNamespace refused when the target dir exists on disk (unmanaged).
	if err := os.MkdirAll(filepath.Join(s.baseDir, "foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameNamespace(ctx, "src", "foreign"); err == nil {
		t.Error("RenameNamespace must be refused when the target dir already exists on disk")
	}
	// `default` is rename-protected, and renaming TO default is refused.
	if err := s.RenameNamespace(ctx, "default", "elsewhere"); err == nil {
		t.Error("renaming the default namespace must be refused")
	}
	if err := s.RenameNamespace(ctx, "src", "default"); err == nil {
		t.Error("renaming a namespace to default must be refused")
	}
	// old==new + invalid name refused.
	if err := s.RenameNamespace(ctx, "src", "src"); err == nil {
		t.Error("RenameNamespace onto the same name must be refused")
	}
	if err := s.RenameNamespace(ctx, "src", "bad name"); err == nil {
		t.Error("RenameNamespace to an invalid name must be refused")
	}

	// A project INSIDE the default namespace renames fine (only the identity `default` is protected).
	seedProject(t, s, "default", "dold", "c.md", "z")
	if err := s.RenameProject(ctx, "default", "dold", "dnew"); err != nil {
		t.Errorf("a project inside the default namespace must rename fine: %v", err)
	}
	if has, _ := s.nsReg.HasProject("default", "dnew"); !has {
		t.Error("the renamed default-namespace project must be registered under its new name")
	}
}

// #6 (core) — locking + AUTOMATIC interrupted-rename recovery for BOTH ops, plus the legacy
// move-journal still recovering as a move.
func TestRename_LockingAndAutoRecovery(t *testing.T) {
	t.Run("namespace quiesce fences writes", func(t *testing.T) {
		s := newEmptyStorage(t)
		seedProject(t, s, "src", "p1", "a.md", "x")
		s.markMovingNs("src")
		if err := s.checkWritable("src", "p1"); !errors.Is(err, ErrProjectMoving) {
			t.Errorf("a write under a renaming namespace must return ErrProjectMoving, got %v", err)
		}
		s.unmarkMovingNs("src")
		if err := s.checkWritable("src", "p1"); errors.Is(err, ErrProjectMoving) {
			t.Error("after unmark, writes must no longer be fenced")
		}
	})

	t.Run("a held lease blocks a namespace rename", func(t *testing.T) {
		s := newEmptyStorage(t)
		ctx := context.Background()
		seedProject(t, s, "src", "p1", "a.md", "x")
		held := make(chan struct{})
		release := make(chan struct{})
		go func() {
			_ = s.locks.WithLock(ctx, "sess", filepath.Join(s.baseDir, "src", "p1", "a.md"), func() error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		if err := s.RenameNamespace(ctx, "src", "dst"); err == nil {
			t.Error("a namespace rename must be refused while a write lease is held on one of its projects")
		}
		close(release)
	})

	t.Run("rename_project forward-completes on restart", func(t *testing.T) {
		dir := t.TempDir()
		ctx := context.Background()
		s1, err := NewFSGitStorageWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		seedProject(t, s1, "ns1", "old", "a.md", "v1")
		// Crash mid-rename: journal at dir_moved, the dir already renamed, registry NOT swapped.
		s1.setOpJournal(nsregistry.OpJournal{
			Op: opRenameProject, OldNamespace: "ns1", OldProject: "old", NewNamespace: "ns1", NewProject: "new", Phase: movePhaseDirMoved,
		})
		s1.evictProjectHandles("ns1", "old")
		if rerr := os.Rename(filepath.Join(dir, "ns1", "old"), filepath.Join(dir, "ns1", "new")); rerr != nil {
			t.Fatal(rerr)
		}
		if cerr := s1.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		s2, err := NewFSGitStorageWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s2.Close() })
		s2.StartupInit(ctx)
		if has, _ := s2.nsReg.HasProject("ns1", "new"); !has {
			t.Error("auto-recovery must finish the registry re-key to the new name")
		}
		if has, _ := s2.nsReg.HasProject("ns1", "old"); has {
			t.Error("auto-recovery must remove the old name from the registry")
		}
		if _, found, _ := s2.nsReg.GetOpJournal(); found {
			t.Error("auto-recovery must clear the op journal")
		}
	})

	t.Run("rename_namespace forward-completes on restart", func(t *testing.T) {
		dir := t.TempDir()
		ctx := context.Background()
		s1, err := NewFSGitStorageWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		seedProject(t, s1, "src", "p1", "a.md", "v1")
		s1.setOpJournal(nsregistry.OpJournal{
			Op: opRenameNamespace, OldNamespace: "src", NewNamespace: "dst", Phase: movePhaseDirMoved,
		})
		s1.evictProjectHandles("src", "p1")
		if rerr := os.Rename(filepath.Join(dir, "src"), filepath.Join(dir, "dst")); rerr != nil {
			t.Fatal(rerr)
		}
		if cerr := s1.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		s2, err := NewFSGitStorageWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s2.Close() })
		s2.StartupInit(ctx)
		if has, _ := s2.nsReg.HasNamespace("dst"); !has {
			t.Error("auto-recovery must finish the namespace re-key to dst")
		}
		if has, _ := s2.nsReg.HasNamespace("src"); has {
			t.Error("auto-recovery must remove the old namespace from the registry")
		}
		if _, found, _ := s2.nsReg.GetOpJournal(); found {
			t.Error("auto-recovery must clear the op journal")
		}
	})

	t.Run("rename_namespace rolls back a pre-rename crash", func(t *testing.T) {
		s := newEmptyStorage(t)
		seedProject(t, s, "src", "p1", "a.md", "v1")
		// Crash BEFORE the rename: journal started, dir still at src.
		s.setOpJournal(nsregistry.OpJournal{
			Op: opRenameNamespace, OldNamespace: "src", NewNamespace: "dst", Phase: movePhaseStarted,
		})
		s.recoverInterruptedOp()
		if _, found, _ := s.nsReg.GetOpJournal(); found {
			t.Error("rollback must clear the journal")
		}
		if has, _ := s.nsReg.HasNamespace("src"); !has {
			t.Error("rollback must keep the namespace at src")
		}
		if has, _ := s.nsReg.HasNamespace("dst"); has {
			t.Error("rollback must not register dst")
		}
	})

	t.Run("legacy move-journal still recovers as a move", func(t *testing.T) {
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
		// A LEGACY journal: Op=="" with only the old `project` field set (no OldProject/NewProject).
		s1.setOpJournal(nsregistry.OpJournal{
			OldNamespace: "src", Project: "proj", NewNamespace: "dst", Phase: movePhaseDirMoved,
		})
		s1.evictProjectHandles("src", "proj")
		if rerr := os.Rename(filepath.Join(dir, "src", "proj"), filepath.Join(dir, "dst", "proj")); rerr != nil {
			t.Fatal(rerr)
		}
		if cerr := s1.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		s2, err := NewFSGitStorageWithOptions(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s2.Close() })
		s2.StartupInit(ctx)
		if has, _ := s2.nsReg.HasProject("dst", "proj"); !has {
			t.Error("a legacy move-journal (Op=\"\") must forward-complete as a move to dst")
		}
		if _, found, _ := s2.nsReg.GetOpJournal(); found {
			t.Error("legacy move recovery must clear the op journal")
		}
	})
}
