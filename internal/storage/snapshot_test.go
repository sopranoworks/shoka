package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type archiveEntry struct {
	typeflag byte
	mode     int64
	content  []byte
	linkname string
}

// readArchive untars a gzip+tar buffer into a path→entry map, failing the test
// on any malformed stream (so "valid archive" is a real assertion).
func readArchive(t *testing.T, data []byte) map[string]archiveEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]archiveEntry{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("tar read %q: %v", hdr.Name, err)
		}
		out[hdr.Name] = archiveEntry{
			typeflag: hdr.Typeflag,
			mode:     hdr.Mode,
			content:  buf.Bytes(),
			linkname: hdr.Linkname,
		}
	}
	return out
}

func snapshotBytes(t *testing.T, s *FSGitStorage, ns, proj string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := s.SnapshotProject(context.Background(), ns, proj, &buf); err != nil {
		t.Fatalf("SnapshotProject: %v", err)
	}
	return buf.Bytes()
}

// TestSnapshotProject_RoundTrip — the archive's paths/bytes/modes exactly match
// the committed HEAD tree, including nested directories.
func TestSnapshotProject_RoundTrip(t *testing.T) {
	s := newTestStorage(t)
	files := map[string]string{
		"a.md":           "# A\n\nalpha\n",
		"dir/b.txt":      "bravo\n",
		"dir/sub/c.json": "{\"k\":1}\n",
	}
	ctx := context.Background()
	for p, c := range files {
		if _, err := s.Write(ctx, "", "ns", "proj", p, c, nil); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	drain(t, s)

	entries := readArchive(t, snapshotBytes(t, s, "ns", "proj"))
	if len(entries) != len(files) {
		t.Fatalf("archive has %d entries, want %d: %v", len(entries), len(files), keysOf(entries))
	}
	for p, want := range files {
		e, ok := entries[p]
		if !ok {
			t.Fatalf("archive missing %q (have %v)", p, keysOf(entries))
		}
		if string(e.content) != want {
			t.Errorf("%q content = %q, want %q", p, e.content, want)
		}
		if e.typeflag != tar.TypeReg {
			t.Errorf("%q typeflag = %d, want regular file", p, e.typeflag)
		}
		if e.mode != 0o644 {
			t.Errorf("%q mode = %o, want 0644", p, e.mode)
		}
	}
}

// TestSnapshotProject_ExcludesUncommitted — a working-tree file that was never
// committed (e.g. a .DS_Store) is absent: the archive is the HEAD tree, not the
// live filesystem.
func TestSnapshotProject_ExcludesUncommitted(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "tracked.md", "x\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// Drop an uncommitted file directly into the project's working tree (the
	// test's own temp dir — not a served data dir). It is never written through
	// the Write path, so it is in neither the WAL nor git.
	noise := filepath.Join(projectWTRoot(s, "ns", "proj"), ".DS_Store")
	if err := os.WriteFile(noise, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := readArchive(t, snapshotBytes(t, s, "ns", "proj"))
	if _, ok := entries[".DS_Store"]; ok {
		t.Fatalf("uncommitted .DS_Store leaked into the archive: %v", keysOf(entries))
	}
	if _, ok := entries["tracked.md"]; !ok {
		t.Fatalf("committed file missing from archive: %v", keysOf(entries))
	}
}

// TestSnapshotProject_ReflectsHeadAfterDrain — after writing a new version and
// draining, the archive carries the new content (HEAD is current post-drain).
func TestSnapshotProject_ReflectsHeadAfterDrain(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "f.md", "v1\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if e := readArchive(t, snapshotBytes(t, s, "ns", "proj"))["f.md"]; string(e.content) != "v1\n" {
		t.Fatalf("snapshot 1 f.md = %q, want v1", e.content)
	}

	if _, err := s.Write(ctx, "", "ns", "proj", "f.md", "v2\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	if e := readArchive(t, snapshotBytes(t, s, "ns", "proj"))["f.md"]; string(e.content) != "v2\n" {
		t.Fatalf("snapshot 2 f.md = %q, want v2", e.content)
	}
}

// TestSnapshotProject_EmptyProject — a project with no committed files yields a
// valid empty archive (no entries), no error, no panic.
func TestSnapshotProject_EmptyProject(t *testing.T) {
	s := newTestStorage(t)
	entries := readArchive(t, snapshotBytes(t, s, "ns", "proj"))
	if len(entries) != 0 {
		t.Fatalf("empty project archive should have no entries, got %v", keysOf(entries))
	}
}

// TestSnapshotProject_CtxCancel — an already-cancelled context aborts cleanly
// with the context error, no panic, no false success.
func TestSnapshotProject_CtxCancel(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "f.md", "x\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.SnapshotProject(ctx, "ns", "proj", io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestSnapshotProject_ConcurrentWithWrites — THE CRUX: SnapshotProject runs
// concurrently with active writes (and their WAL commits) to the SAME project
// under -race. Every snapshot is a VALID archive of SOME consistent HEAD (a
// stable, never-rewritten file appears with its exact content in every one), and
// BOTH the writer and the snapshotter run to completion — neither blocks the
// other. Real post-conditions (B-29), no sleeps.
func TestSnapshotProject_ConcurrentWithWrites(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "keep.md", "k\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, "", "ns", "proj", "churn.md", "v0\n", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	const writes = 150
	const snaps = 150
	var writesDone, snapsDone atomic.Int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if _, err := s.Write(ctx, "", "ns", "proj", "churn.md", fmt.Sprintf("v%d\n", i), nil); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
			writesDone.Add(1)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < snaps; i++ {
			var buf bytes.Buffer
			if err := s.SnapshotProject(ctx, "ns", "proj", &buf); err != nil {
				t.Errorf("snapshot %d: %v", i, err)
				return
			}
			entries := readArchive(t, buf.Bytes()) // fails the test if malformed
			e, ok := entries["keep.md"]
			if !ok || string(e.content) != "k\n" {
				t.Errorf("snapshot %d: stable keep.md missing/wrong (ok=%v, content=%q)", i, ok, e.content)
				return
			}
			snapsDone.Add(1)
		}
	}()

	wg.Wait()
	if writesDone.Load() != writes {
		t.Fatalf("writer made %d/%d writes — snapshot blocked it", writesDone.Load(), writes)
	}
	if snapsDone.Load() != snaps {
		t.Fatalf("snapshotter made %d/%d snapshots — writes blocked it", snapsDone.Load(), snaps)
	}
	assertNoViolations(t, s, "ns", "proj")
}

func keysOf(m map[string]archiveEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
