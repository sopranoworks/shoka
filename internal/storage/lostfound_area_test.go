package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
