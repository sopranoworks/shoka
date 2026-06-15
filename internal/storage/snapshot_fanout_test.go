package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeDrainP(t *testing.T, s *FSGitStorage, ns, proj, path, content string) {
	t.Helper()
	if _, err := s.Write(context.Background(), "", ns, proj, path, content, nil); err != nil {
		t.Fatalf("write %s/%s/%s: %v", ns, proj, path, err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain")
	}
}

// hasTempLeftover reports whether dir contains any temp snapshot file.
func hasTempLeftover(t *testing.T, dir string) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			return true
		}
	}
	return false
}

// TestSnapshotProjectToDir_Single — writes <output>/<ns>/<proj>/<ts>.tar.gz,
// matching the HEAD tree; no temp file remains.
func TestSnapshotProjectToDir_Single(t *testing.T) {
	s := newTestStorage(t)
	writeDrainP(t, s, "ns", "proj", "a.md", "alpha\n")
	writeDrainP(t, s, "ns", "proj", "dir/b.txt", "bravo\n")
	out := t.TempDir()

	path, err := s.SnapshotProjectToDir(context.Background(), "ns", "proj", out)
	require.NoError(t, err)

	dir := filepath.Join(out, "ns", "proj")
	require.Equal(t, dir, filepath.Dir(path))
	require.True(t, strings.HasSuffix(path, ".tar.gz"))
	base := strings.TrimSuffix(filepath.Base(path), ".tar.gz")
	_, perr := time.Parse(snapshotTimeFormat, base)
	require.NoError(t, perr, "filename stem must be a valid <ts>")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	entries := readArchive(t, data)
	assert.Equal(t, "alpha\n", string(entries["a.md"].content))
	assert.Equal(t, "bravo\n", string(entries["dir/b.txt"].content))
	assert.False(t, hasTempLeftover(t, dir), "no temp file should remain after success")
}

// TestSnapshotProjectToDir_AtomicNoPartialOnFailure — a failure (an already-
// cancelled ctx aborting SnapshotProject) leaves NO final .tar.gz and NO temp.
func TestSnapshotProjectToDir_AtomicNoPartialOnFailure(t *testing.T) {
	s := newTestStorage(t)
	writeDrainP(t, s, "ns", "proj", "a.md", "alpha\n")
	out := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.SnapshotProjectToDir(ctx, "ns", "proj", out)
	require.Error(t, err)

	dir := filepath.Join(out, "ns", "proj")
	ents, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	for _, e := range ents {
		t.Fatalf("expected an empty dir after a failed snapshot, found %q", e.Name())
	}
}

// TestSnapshotScope_NamespaceFanout — a namespace with ≥2 projects yields an
// archive per project; .shoka-lostfound / non-projects are excluded.
func TestSnapshotScope_NamespaceFanout(t *testing.T) {
	s, base := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "p1"))
	require.NoError(t, s.CreateProject("ns", "p2"))
	writeDrainP(t, s, "ns", "p1", "f.md", "one\n")
	writeDrainP(t, s, "ns", "p2", "f.md", "two\n")
	// A quarantine area under the namespace — must NOT be snapshotted.
	require.NoError(t, os.MkdirAll(filepath.Join(base, "ns", ".shoka-lostfound", "p1"), 0o755))
	out := t.TempDir()

	results, err := s.SnapshotScope(context.Background(), Scope{Namespace: "ns"}, out)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, r := range results {
		require.NoError(t, r.Err, "%s/%s", r.Namespace, r.Project)
		got[r.Project] = true
		_, statErr := os.Stat(r.Path)
		require.NoError(t, statErr, "archive should exist for %s", r.Project)
	}
	assert.Equal(t, map[string]bool{"p1": true, "p2": true}, got)
	_, lfErr := os.Stat(filepath.Join(out, "ns", ".shoka-lostfound"))
	assert.True(t, os.IsNotExist(lfErr), ".shoka-lostfound must never be snapshotted")
}

// TestSnapshotScope_WholeStoreResilient — ≥2 namespaces; one project's failure
// (its output path is blocked by a pre-existing file) does NOT abort the rest.
func TestSnapshotScope_WholeStoreResilient(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns1", "a"))
	require.NoError(t, s.CreateProject("ns2", "b"))
	writeDrainP(t, s, "ns1", "a", "f.md", "aaa\n")
	writeDrainP(t, s, "ns2", "b", "f.md", "bbb\n")
	out := t.TempDir()

	// Block ns2/b: place a FILE where its project dir must be created, so MkdirAll
	// fails for that project only.
	require.NoError(t, os.MkdirAll(filepath.Join(out, "ns2"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(out, "ns2", "b"), []byte("x"), 0o644))

	results, err := s.SnapshotScope(context.Background(), Scope{}, out)
	require.Error(t, err, "aggregate error should report the failed project")

	byProj := map[string]SnapshotResult{}
	for _, r := range results {
		byProj[r.Namespace+"/"+r.Project] = r
	}
	require.Contains(t, byProj, "ns1/a")
	require.Contains(t, byProj, "ns2/b")
	assert.NoError(t, byProj["ns1/a"].Err, "the healthy project must still succeed")
	_, statErr := os.Stat(byProj["ns1/a"].Path)
	assert.NoError(t, statErr)
	assert.Error(t, byProj["ns2/b"].Err, "the blocked project must report its error")
}

// makeSnapshotFiles creates empty <ts>.tar.gz files under <out>/<ns>/<proj> with
// the given timestamps, returning their names (sorted).
func makeSnapshotFiles(t *testing.T, out, ns, proj string, times []time.Time) []string {
	t.Helper()
	dir := filepath.Join(out, ns, proj)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	var names []string
	for _, tm := range times {
		name := tm.UTC().Format(snapshotTimeFormat) + ".tar.gz"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func remainingNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestPruneSnapshots_Count — keepCount=N over N+2 snapshots ⇒ the N newest remain.
func TestPruneSnapshots_Count(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	out := t.TempDir()

	const keep = 3
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var times []time.Time
	for i := 0; i < keep+2; i++ {
		times = append(times, base.Add(time.Duration(i)*time.Hour))
	}
	makeSnapshotFiles(t, out, "ns", "proj", times)

	removed, err := s.PruneSnapshots(out, Scope{Namespace: "ns", Project: "proj"}, keep, 0)
	require.NoError(t, err)
	assert.Len(t, removed, 2, "the 2 oldest should be removed")

	remaining := remainingNames(t, filepath.Join(out, "ns", "proj"))
	require.Len(t, remaining, keep)
	// The kept ones are the newest <ts> (last `keep` of the sorted list).
	for i := 2; i < len(times); i++ {
		want := times[i].UTC().Format(snapshotTimeFormat) + ".tar.gz"
		assert.Contains(t, remaining, want)
	}
}

// TestPruneSnapshots_Age — maxAge removes only the too-old snapshots.
func TestPruneSnapshots_Age(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	out := t.TempDir()

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // far past
	recent := time.Now().UTC().Add(-time.Minute)
	makeSnapshotFiles(t, out, "ns", "proj", []time.Time{old, recent})

	removed, err := s.PruneSnapshots(out, Scope{Namespace: "ns", Project: "proj"}, 0, time.Hour)
	require.NoError(t, err)
	require.Len(t, removed, 1)
	assert.Contains(t, removed[0], old.Format(snapshotTimeFormat))

	remaining := remainingNames(t, filepath.Join(out, "ns", "proj"))
	assert.Equal(t, []string{recent.Format(snapshotTimeFormat) + ".tar.gz"}, remaining)
}

// TestPruneSnapshots_Safety — non-snapshot files are NEVER deleted.
func TestPruneSnapshots_Safety(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	out := t.TempDir()
	dir := filepath.Join(out, "ns", "proj")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	makeSnapshotFiles(t, out, "ns", "proj", []time.Time{base, base.Add(time.Hour)})
	// Decoys: a plain file and a .tar.gz whose stem is not a valid <ts>.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manual-backup.tar.gz"), []byte("keep"), 0o644))

	// keepCount=1 ⇒ the older valid snapshot is removed; decoys untouched.
	removed, err := s.PruneSnapshots(out, Scope{Namespace: "ns", Project: "proj"}, 1, 0)
	require.NoError(t, err)
	require.Len(t, removed, 1)

	remaining := remainingNames(t, dir)
	assert.Contains(t, remaining, "notes.txt")
	assert.Contains(t, remaining, "manual-backup.tar.gz")
	assert.Contains(t, remaining, base.Add(time.Hour).Format(snapshotTimeFormat)+".tar.gz")
	assert.NotContains(t, remaining, base.Format(snapshotTimeFormat)+".tar.gz")
}

// TestSnapshotScope_CtxCancel — an already-cancelled context stops the fan-out
// cleanly, returning the ctx error and (here) no partial results.
func TestSnapshotScope_CtxCancel(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "p1"))
	require.NoError(t, s.CreateProject("ns", "p2"))
	writeDrainP(t, s, "ns", "p1", "f.md", "x\n")
	out := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := s.SnapshotScope(ctx, Scope{Namespace: "ns"}, out)
	require.True(t, errors.Is(err, context.Canceled), "got %v", err)
	assert.Empty(t, results, "cancellation before the first project yields no results")
}
