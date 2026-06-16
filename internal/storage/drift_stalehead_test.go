package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/storage/catalog"
)

// These tests pin the stale-HEAD `corrupted` defect (2026-06-16 directive): an
// EXTERNAL git HEAD move (a host `git reset`, the documented out-of-band "git add"
// landing) changes the working tree without updating Shoka's per-project catalog,
// leaving stale etags that DetectDrift reported as a false `corrupted` — permanently,
// because the catalog `.db` persists across restart. The fix reconciles a divergent
// catalog against the LIVE on-disk git HEAD: a tree that is clean vs HEAD is restored
// to healthy and the catalog rebuilt from HEAD; only genuine tree-vs-HEAD drift stays
// corrupted.
//
// The external actor is the host `git` CLI (os/exec) — not go-git. That is both the
// faithful reproduction of the real trigger AND a hard requirement here: archlint
// (TestNoNonAtomicRefWritesInStorage) forbids go-git porcelain ref writes anywhere in
// internal/storage, test files included, so a test could not call w.Reset/w.Commit.

// runGitExternal runs the host git CLI against an existing project repo, standing in
// for an out-of-band actor (operator on the host, a mobile "git add" landing). A
// non-zero exit fails the test.
func runGitExternal(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=External Actor", "GIT_AUTHOR_EMAIL=ext@example.com",
		"GIT_COMMITTER_NAME=External Actor", "GIT_COMMITTER_EMAIL=ext@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external git %v failed: %v\n%s", args, err, out)
	}
}

// TestStaleHead_ExternalReset_WriteSucceeds is core RED→GREEN test #1: after a host
// `git reset --hard HEAD~1` the working tree is clean against the new HEAD, but the
// catalog still references the rewound file. A scan (DetectDrift) must NOT mark the
// project corrupted, and a subsequent write must succeed.
//
// RED (pre-fix): DetectDrift compares the stale catalog to the tree → false
// corrupted; the write is refused with ErrProjectCorrupted and the lazy rescan
// re-confirms it against the same stale catalog.
func TestStaleHead_ExternalReset_WriteSucceeds(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()
	projectPath := filepath.Join(s.baseDir, "ns", "proj")

	if _, err := s.Write(ctx, "", "ns", "proj", "keep.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "rewound.md", "to-be-removed", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// External actor rewinds HEAD by one commit: rewound.md leaves both the working
	// tree and HEAD; the tree is now clean against the new HEAD.
	runGitExternal(t, projectPath, "reset", "--hard", "HEAD~1")

	// A scan (the periodic drift detector / a restart) runs DetectDrift. Post-fix it
	// reconciles against the live HEAD and reports healthy; pre-fix it reported
	// corrupted (the RED point).
	sum, err := s.DetectDrift("ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if sum.State != StateHealthy {
		t.Fatalf("after external reset on a CLEAN tree, state = %s, want healthy (false stale-HEAD corrupted)", sum.State)
	}

	// The core guarantee: a write succeeds against the new HEAD.
	if _, err := s.Write(ctx, "", "ns", "proj", "fresh.md", "after-reset", nil); err != nil {
		t.Fatalf("write after external reset was refused (stale-HEAD corrupted not fixed): %v", err)
	}
	drain(t, s)

	// The rewound file is gone; the kept and fresh files are present and correct.
	if got, err := s.ReadFile("ns", "proj", "keep.md"); err != nil || got != "v1" {
		t.Fatalf("keep.md after reset = %q, err=%v; want v1", got, err)
	}
	if got, err := s.ReadFile("ns", "proj", "fresh.md"); err != nil || got != "after-reset" {
		t.Fatalf("fresh.md = %q, err=%v; want after-reset", got, err)
	}
	if _, err := s.ReadFile("ns", "proj", "rewound.md"); err == nil {
		t.Fatal("rewound.md should be absent after the external reset")
	}
}

// TestStaleHead_ExternalCommit_WriteSucceeds is core test #1's second case: an
// out-of-band commit that EDITS an existing tracked file (the documented mobile
// "git add" + commit landing). HEAD advances, the tree is clean against it, but the
// catalog holds the pre-edit etag → false corrupted before the fix.
func TestStaleHead_ExternalCommit_WriteSucceeds(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()
	projectPath := filepath.Join(s.baseDir, "ns", "proj")

	if _, err := s.Write(ctx, "", "ns", "proj", "doc.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// External actor edits the tracked file on disk and lands it as a commit.
	if err := os.WriteFile(filepath.Join(projectPath, "doc.md"), []byte("v2-landed-externally"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitExternal(t, projectPath, "commit", "-am", "external landing")

	sum, err := s.DetectDrift("ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if sum.State != StateHealthy {
		t.Fatalf("after out-of-band commit on a CLEAN tree, state = %s, want healthy", sum.State)
	}

	if _, err := s.Write(ctx, "", "ns", "proj", "next.md", "ok", nil); err != nil {
		t.Fatalf("write after external commit refused: %v", err)
	}
	drain(t, s)

	// The externally-landed content is what reads return.
	if got, err := s.ReadFile("ns", "proj", "doc.md"); err != nil || got != "v2-landed-externally" {
		t.Fatalf("doc.md = %q, err=%v; want the externally-landed v2", got, err)
	}
}

// TestStaleHead_ReadsStayCorrect is the guard from the directive: reads were always
// correct (they cross-check disk), and must remain so after the external move.
func TestStaleHead_ReadsStayCorrect(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()
	projectPath := filepath.Join(s.baseDir, "ns", "proj")

	if _, err := s.Write(ctx, "", "ns", "proj", "keep.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "rewound.md", "x", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	runGitExternal(t, projectPath, "reset", "--hard", "HEAD~1")

	files, _, err := s.ListFiles("ns", "proj", "")
	if err != nil {
		t.Fatal(err)
	}
	has := func(name string) bool {
		for _, f := range files {
			if f == name {
				return true
			}
		}
		return false
	}
	if !has("keep.md") {
		t.Errorf("listing should contain keep.md after reset; got %v", files)
	}
	if has("rewound.md") {
		t.Errorf("listing should NOT contain the rewound file after reset; got %v", files)
	}
	if got, err := s.ReadFile("ns", "proj", "keep.md"); err != nil || got != "v1" {
		t.Fatalf("read keep.md = %q, err=%v; want v1", got, err)
	}
}

// TestStaleHead_ResyncToHead_ClearsStuck is core test #3: force the stale-baseline
// condition directly (poison a catalog etag so the file reads as drifted while the
// working tree is clean vs HEAD), pin the project to corrupted, then the explicit
// recovery operation ResyncToHead — the one the MCP `recover_project` tool and the
// Web UI recover action invoke — clears it and a write succeeds.
func TestStaleHead_ResyncToHead_ClearsStuck(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()

	if _, err := s.Write(ctx, "", "ns", "proj", "doc.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// Force the stale baseline: the working tree is clean vs HEAD, but the catalog
	// records a wrong etag for doc.md (exactly the shape an external HEAD move
	// leaves). VerifyInvariant will report etag_mismatch.
	cat, err := s.catalogFor("ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if perr := cat.PutFile("doc.md", catalog.FileEntry{Etag: "stale-etag-from-a-superseded-head", Size: 2, ModifiedAt: time.Now().UTC()}); perr != nil {
		t.Fatal(perr)
	}
	s.setState("ns", "proj", StateCorrupted)
	if s.State("ns", "proj") != StateCorrupted {
		t.Fatal("precondition: project should be pinned corrupted")
	}

	state, err := s.ResyncToHead("ns", "proj")
	if err != nil {
		t.Fatalf("ResyncToHead: %v", err)
	}
	if state != StateHealthy {
		t.Fatalf("ResyncToHead returned %s, want healthy (a clean-on-disk project must recover)", state)
	}
	if s.State("ns", "proj") != StateHealthy {
		t.Fatalf("state after ResyncToHead = %s, want healthy", s.State("ns", "proj"))
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "after.md", "ok", nil); err != nil {
		t.Fatalf("write after recovery refused: %v", err)
	}
}

// TestStaleHead_RestartCleanDisk_Healthy is core test #4: a restart over a clean
// working tree must derive a non-corrupted state. The catalog `.db` persists across
// the restart with its stale entries; the new instance's startup must reconcile it
// against the on-disk HEAD rather than re-stranding the project in corrupted.
func TestStaleHead_RestartCleanDisk_Healthy(t *testing.T) {
	ctx := context.Background()
	baseDir, err := os.MkdirTemp("", "shoka-stalehead-restart-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })
	opts := Options{Identity: identity.Defaults{UserName: "Osamu Takahashi", UserEmail: "forte.nit@gmail.com", AgentName: "shoka-agent"}}

	// Instance 1: create, write two commits, then close (releases the catalog .db).
	s1, err := NewFSGitStorageWithOptions(baseDir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Write(ctx, "", "ns", "proj", "keep.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Write(ctx, "", "ns", "proj", "rewound.md", "x", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s1)
	if !s1.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain before close")
	}
	_ = s1.Close()

	// External actor rewinds HEAD while the server is down; the tree stays clean.
	runGitExternal(t, filepath.Join(baseDir, "ns", "proj"), "reset", "--hard", "HEAD~1")

	// Instance 2 = the restart. The persisted catalog still lists rewound.md.
	s2, err := NewFSGitStorageWithOptions(baseDir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.StartupInit(ctx)

	if got := s2.State("ns", "proj"); got != StateHealthy {
		t.Fatalf("after restart over a clean tree, state = %s, want healthy (restart must not strand corrupted)", got)
	}
	if _, err := s2.Write(ctx, "", "ns", "proj", "fresh.md", "ok", nil); err != nil {
		t.Fatalf("write after restart refused: %v", err)
	}
}

// TestStaleHead_GenuineDrift_StillCorrupted is core test #5: the fix must NOT mask
// real corruption. An uncommitted hand-edit of a tracked file (the tree genuinely
// diverges from HEAD) still reports corrupted, the write is still refused, and
// ResyncToHead does NOT paper over it.
func TestStaleHead_GenuineDrift_StillCorrupted(t *testing.T) {
	s := newIdentityStorage(t)
	ctx := context.Background()
	projectPath := filepath.Join(s.baseDir, "ns", "proj")

	if _, err := s.Write(ctx, "", "ns", "proj", "doc.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// Genuine drift: a tracked file is hand-edited on disk and NOT committed.
	if err := os.WriteFile(filepath.Join(projectPath, "doc.md"), []byte("hand-edited-uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := s.DetectDrift("ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if sum.State != StateCorrupted {
		t.Fatalf("genuine uncommitted drift: state = %s, want corrupted", sum.State)
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "other.md", "x", nil); err == nil {
		t.Fatal("write to a genuinely-corrupted project should be refused")
	}
	// The non-destructive recovery does not silently adopt the divergence.
	if state, _ := s.ResyncToHead("ns", "proj"); state != StateCorrupted {
		t.Fatalf("ResyncToHead over genuine drift = %s, want corrupted (must not paper over real divergence)", state)
	}
}
