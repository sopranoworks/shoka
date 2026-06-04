package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/notify"
)

var fixedTS = time.Date(2026, 6, 2, 11, 55, 50, 0, time.UTC)

func TestMoveToLostFound_PreservesContentOutsideRepo(t *testing.T) {
	s := newIdentityStorage(t)
	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	src := filepath.Join(projectPath, "mystery.md")
	if err := os.WriteFile(src, []byte("unknown content"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest, err := s.moveToLostFound("ns", "proj", "mystery.md", fixedTS)
	if err != nil {
		t.Fatalf("moveToLostFound: %v", err)
	}

	// Source removed from the working tree.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("expected source removed, stat err=%v", err)
	}
	// Destination holds the original content.
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "unknown content" {
		t.Fatalf("content not preserved: %q", got)
	}
	// Destination is under the project's lost+found area, NOT inside the repo.
	wantPrefix := filepath.Join(s.baseDir, "ns", ".shoka-lostfound", "proj")
	if !strings.HasPrefix(dest, wantPrefix) {
		t.Fatalf("dest %q not under lost+found area %q", dest, wantPrefix)
	}
	if strings.HasPrefix(dest, projectPath+string(os.PathSeparator)) {
		t.Fatalf("dest %q must not be inside the project repo", dest)
	}
}

func TestMoveToLostFound_NestedPathPreserved(t *testing.T) {
	s := newIdentityStorage(t)
	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	if err := os.MkdirAll(filepath.Join(projectPath, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "sub", "dir", "x.md"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest, err := s.moveToLostFound("ns", "proj", "sub/dir/x.md", fixedTS)
	if err != nil {
		t.Fatalf("moveToLostFound: %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(dest), "sub/dir/x.md") {
		t.Fatalf("nested rel path not preserved in dest: %q", dest)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "nested" {
		t.Fatalf("content not preserved: %q", got)
	}
}

func TestMoveToLostFound_CollisionDisambiguated(t *testing.T) {
	s := newIdentityStorage(t)
	projectPath := filepath.Join(s.baseDir, "ns", "proj")

	// First file at rel "dup.md".
	if err := os.WriteFile(filepath.Join(projectPath, "dup.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest1, err := s.moveToLostFound("ns", "proj", "dup.md", fixedTS)
	if err != nil {
		t.Fatalf("move 1: %v", err)
	}
	// A new file later appears at the same rel; same timestamp forces a collision.
	if err := os.WriteFile(filepath.Join(projectPath, "dup.md"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest2, err := s.moveToLostFound("ns", "proj", "dup.md", fixedTS)
	if err != nil {
		t.Fatalf("move 2: %v", err)
	}

	if dest1 == dest2 {
		t.Fatalf("collision not disambiguated: both at %q", dest1)
	}
	if c1, _ := os.ReadFile(dest1); string(c1) != "first" {
		t.Fatalf("dest1 content = %q, want first", c1)
	}
	if c2, _ := os.ReadFile(dest2); string(c2) != "second" {
		t.Fatalf("dest2 content = %q, want second", c2)
	}
}

// --- write-bytes deposit (D3) ---

// TestDepositBytes_RepoAbsent is the load-bearing D2 property: write-bytes must
// succeed when the project's repo — indeed its whole directory — does not exist,
// because the lost+found area lives under the namespace root, outside any repo.
// This is exactly D3's case: a WAL entry for a repo-less project.
func TestDepositBytes_RepoAbsent(t *testing.T) {
	s := newIdentityStorage(t) // creates ns/proj; "ghost" below is never created
	ghostDir := filepath.Join(s.baseDir, "ns", "ghost")
	if _, err := os.Stat(ghostDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: ghost project dir must not exist, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(ghostDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("precondition: ghost project repo must not exist")
	}

	dest, err := s.depositBytes("ns", "ghost", "doc/spec.md", []byte("uncommittable"), fixedTS)
	if err != nil {
		t.Fatalf("depositBytes with repo absent: %v", err)
	}

	wantPrefix := filepath.Join(s.baseDir, "ns", ".shoka-lostfound", "ghost")
	if !strings.HasPrefix(dest, wantPrefix) {
		t.Fatalf("dest %q not under lost+found area %q", dest, wantPrefix)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "uncommittable" {
		t.Fatalf("content not preserved: %q", got)
	}
	if !strings.HasSuffix(filepath.ToSlash(dest), "doc/spec.md") {
		t.Fatalf("original path not preserved in dest: %q", dest)
	}
	// Depositing must not have created the project repo.
	if _, err := os.Stat(ghostDir); !os.IsNotExist(err) {
		t.Fatalf("depositBytes must not create the project dir, stat err=%v", err)
	}
}

func TestDepositBytes_CollisionDisambiguated(t *testing.T) {
	s := newIdentityStorage(t)
	dest1, err := s.depositBytes("ns", "proj", "a.md", []byte("first"), fixedTS)
	if err != nil {
		t.Fatalf("deposit 1: %v", err)
	}
	dest2, err := s.depositBytes("ns", "proj", "a.md", []byte("second"), fixedTS)
	if err != nil {
		t.Fatalf("deposit 2: %v", err)
	}
	if dest1 == dest2 {
		t.Fatalf("collision not disambiguated: both at %q", dest1)
	}
	if c1, _ := os.ReadFile(dest1); string(c1) != "first" {
		t.Fatalf("dest1 = %q, want first", c1)
	}
	if c2, _ := os.ReadFile(dest2); string(c2) != "second" {
		t.Fatalf("dest2 = %q, want second", c2)
	}
}

// --- move-tree deposit (D4) ---

// TestDepositTree_MovesTreeAndSiblingAtomic moves a whole repo-less tree plus a
// sibling file into one <ts> dir (D4's leftover <project>/ tree + its <project>.db).
func TestDepositTree_MovesTreeAndSiblingAtomic(t *testing.T) {
	s := newIdentityStorage(t)
	nsRoot := filepath.Join(s.baseDir, "ns")

	// A repo-less leftover tree with a nested file, plus a sibling .db.
	srcTree := filepath.Join(nsRoot, "leftover")
	if err := os.MkdirAll(filepath.Join(srcTree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcTree, "sub", "note.md"), []byte("tree content"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(nsRoot, "leftover.db")
	if err := os.WriteFile(sibling, []byte("catalog db"), 0o644); err != nil {
		t.Fatal(err)
	}

	tsDir, err := s.depositTree("ns", "leftover", srcTree, fixedTS, sibling)
	if err != nil {
		t.Fatalf("depositTree: %v", err)
	}

	// Both grouped under one <ts> dir.
	wantTS := filepath.Join(nsRoot, ".shoka-lostfound", "leftover", fixedTS.Format(lostFoundTimeFormat))
	if tsDir != wantTS {
		t.Fatalf("ts dir = %q, want %q", tsDir, wantTS)
	}
	// Sources gone after the atomic rename.
	if _, err := os.Stat(srcTree); !os.IsNotExist(err) {
		t.Fatalf("source tree must be gone, stat err=%v", err)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Fatalf("source sibling must be gone, stat err=%v", err)
	}
	// Tree (with nested content) landed under the <ts> dir.
	movedNote := filepath.Join(tsDir, "leftover", "sub", "note.md")
	if c, err := os.ReadFile(movedNote); err != nil || string(c) != "tree content" {
		t.Fatalf("moved tree content: err=%v content=%q", err, c)
	}
	// Sibling landed alongside, under the same <ts> dir.
	movedDB := filepath.Join(tsDir, "leftover.db")
	if c, err := os.ReadFile(movedDB); err != nil || string(c) != "catalog db" {
		t.Fatalf("moved sibling content: err=%v content=%q", err, c)
	}
}

func TestDepositTree_CollisionDisambiguated(t *testing.T) {
	s := newIdentityStorage(t)
	nsRoot := filepath.Join(s.baseDir, "ns")

	mk := func(content string) string {
		d := filepath.Join(nsRoot, "dup")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}

	ts1, err := s.depositTree("ns", "dup", mk("first"), fixedTS)
	if err != nil {
		t.Fatalf("deposit 1: %v", err)
	}
	ts2, err := s.depositTree("ns", "dup", mk("second"), fixedTS)
	if err != nil {
		t.Fatalf("deposit 2: %v", err)
	}
	// Same <ts> dir (same timestamp), but the tree names disambiguate within it.
	if ts1 != ts2 {
		t.Fatalf("expected same <ts> dir, got %q vs %q", ts1, ts2)
	}
	if c, _ := os.ReadFile(filepath.Join(ts1, "dup", "f.md")); string(c) != "first" {
		t.Fatalf("first tree = %q, want first", c)
	}
	if c, _ := os.ReadFile(filepath.Join(ts1, "dup.1", "f.md")); string(c) != "second" {
		t.Fatalf("disambiguated tree = %q, want second", c)
	}
}

// --- lostfound.quarantined NOTIFY (D3/D4) ---

// TestNotifyQuarantined_FixedShape asserts the new kind reuses the fixed 5-field
// Event shape with no SourcePath — no wire-shape change.
func TestNotifyQuarantined_FixedShape(t *testing.T) {
	center := notify.NewCenter(16)
	s := newStorageWithCenter(t, center)

	s.notifyQuarantined("ns", "proj", "doc/spec.md")

	events := center.Snapshot()
	var ev *notify.Event
	for i := range events {
		if events[i].Kind == kindLostFoundQuarantined {
			ev = &events[i]
			break
		}
	}
	if ev == nil {
		t.Fatalf("no %s event recorded; got %d events", kindLostFoundQuarantined, len(events))
	}
	if ev.Kind != "lostfound.quarantined" {
		t.Fatalf("kind = %q, want lostfound.quarantined", ev.Kind)
	}
	if ev.Target != "ns/proj" {
		t.Fatalf("target = %q, want ns/proj", ev.Target)
	}
	if ev.Path != "doc/spec.md" {
		t.Fatalf("path = %q, want doc/spec.md", ev.Path)
	}
	if ev.SourcePath != "" {
		t.Fatalf("SourcePath = %q, want empty (fixed 5-field shape, no wire change)", ev.SourcePath)
	}
	if ev.Seq == 0 || ev.Timestamp.IsZero() {
		t.Fatalf("Seq/Timestamp not stamped: seq=%d ts=%v", ev.Seq, ev.Timestamp)
	}
}
