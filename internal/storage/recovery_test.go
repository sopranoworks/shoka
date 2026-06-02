package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepairTrackedChanges_DoesNotAdoptUntracked is the regression pin for the
// 2026-06-01 contamination: the old accept-working-tree recovery used
// AddOptions{All:true} ("git add -A") and swept an untracked .DS_Store into a
// commit (rohrpost-dev 3896bcd). The fixed RepairTrackedChanges uses tracked-only
// staging ("git commit -a"), so a working tree carrying an untracked .DS_Store
// alongside a real tracked modification produces a commit that contains the
// tracked change but NOT the .DS_Store. This reproduces the real contamination
// shape (tracked file changed + untracked junk present) against a fresh project.
func TestRepairTrackedChanges_DoesNotAdoptUntracked(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()

	// Two committed tracked files.
	if _, err := s.Write(ctx, "", "ns", "proj", "keep.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "gone.md", "bye", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	// Drift the working tree outside Shoka's write path: modify a tracked file,
	// delete another tracked file, and drop an untracked .DS_Store (the junk).
	if err := os.WriteFile(filepath.Join(projectPath, "keep.md"), []byte("v2-hand-edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(projectPath, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, ".DS_Store"), []byte("\x00\x00junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The project is corrupted (a tracked modification/deletion); recover it.
	if got := s.State("ns", "proj"); got != StateCorrupted {
		// DetectDrift sets the state; run it so the precondition is explicit.
		sum, err := s.DetectDrift("ns", "proj")
		if err != nil {
			t.Fatal(err)
		}
		if sum.State != StateCorrupted {
			t.Fatalf("precondition: expected corrupted, got %s", sum.State)
		}
	}

	hash, err := s.RepairTrackedChanges(ctx, "ns", "proj")
	if err != nil {
		t.Fatalf("RepairTrackedChanges: %v", err)
	}
	if hash == "" {
		t.Fatal("expected a recovery commit (tracked changes were present)")
	}

	c := headCommit(t, s)

	// The fix: the recovery commit's tree must NOT contain the untracked .DS_Store.
	if _, err := c.File(".DS_Store"); err == nil {
		t.Error("recovery commit adopted the untracked .DS_Store — the contamination was NOT fixed")
	}
	// It must contain the tracked modification...
	f, err := c.File("keep.md")
	if err != nil {
		t.Fatalf("recovery commit is missing the tracked file keep.md: %v", err)
	}
	if got, _ := f.Contents(); got != "v2-hand-edited" {
		t.Errorf("keep.md content = %q, want the hand-edited tracked change", got)
	}
	// ...and must record the tracked deletion.
	if _, err := c.File("gone.md"); err == nil {
		t.Error("recovery commit did not stage the deletion of the tracked file gone.md")
	}

	// The .DS_Store is left in place on disk (untracked, not committed) — recovery
	// neither adopts nor deletes it.
	if _, err := os.Stat(filepath.Join(projectPath, ".DS_Store")); err != nil {
		t.Errorf("untracked .DS_Store should remain on disk after RepairTrackedChanges: %v", err)
	}

	// Commit attribution: shoka-recovery system agent as author, configured user
	// as committer (per identity-config), and the accurate new subject line.
	if c.Author.Name != "shoka-recovery" {
		t.Errorf("recovery author = %q, want shoka-recovery", c.Author.Name)
	}
	if c.Committer.Name != "Osamu Takahashi" {
		t.Errorf("recovery committer = %q, want the configured user", c.Committer.Name)
	}
	if !strings.HasPrefix(c.Message, "Recovery: tracked changes adopted") {
		t.Errorf("recovery subject = %q, want the tracked-only phrasing", c.Message)
	}

	// Recovery returns the project to healthy.
	if got := s.State("ns", "proj"); got != StateHealthy {
		t.Errorf("state after recovery = %s, want healthy", got)
	}
}

// TestRestoreToLatest_RemovesUntracked confirms the sibling intent: accept-head
// discards tracked changes and removes untracked files (the .DS_Store is deleted,
// not committed) — the inverse of contamination, and permitted.
func TestRestoreToLatest_RemovesUntracked(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "keep.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	if err := os.WriteFile(filepath.Join(projectPath, "keep.md"), []byte("v2-hand-edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreToLatest(ctx, "ns", "proj"); err != nil {
		t.Fatalf("RestoreToLatest: %v", err)
	}

	// Tracked change discarded back to HEAD.
	got, err := os.ReadFile(filepath.Join(projectPath, "keep.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("keep.md = %q, want v1 (restored to HEAD)", got)
	}
	// Untracked junk removed.
	if _, err := os.Stat(filepath.Join(projectPath, ".DS_Store")); !os.IsNotExist(err) {
		t.Errorf("untracked .DS_Store should have been cleaned by RestoreToLatest (err=%v)", err)
	}
	if got := s.State("ns", "proj"); got != StateHealthy {
		t.Errorf("state after restore = %s, want healthy", got)
	}
}
