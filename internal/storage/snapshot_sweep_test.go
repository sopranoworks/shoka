package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseScope(t *testing.T) {
	cases := []struct {
		in   string
		want Scope
		err  bool
	}{
		{"", Scope{}, false},
		{"all", Scope{}, false},
		{"namespace:shoka", Scope{Namespace: "shoka"}, false},
		{"project:shoka/maintenance", Scope{Namespace: "shoka", Project: "maintenance"}, false},
		{"namespace:", Scope{}, true},
		{"project:shoka", Scope{}, true},
		{"project:/maintenance", Scope{}, true},
		{"bogus", Scope{}, true},
	}
	for _, c := range cases {
		got, err := ParseScope(c.in)
		if c.err {
			assert.Error(t, err, "ParseScope(%q)", c.in)
			continue
		}
		require.NoError(t, err, "ParseScope(%q)", c.in)
		assert.Equal(t, c.want, got, "ParseScope(%q)", c.in)
	}
}

// TestStartSnapshotSweep_DisabledNoOp — a disabled (or interval<=0) sweep starts
// no goroutine and produces nothing; the output dir is never created.
func TestStartSnapshotSweep_DisabledNoOp(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()

	for _, cfg := range []SnapshotSweepConfig{
		{Enabled: false, Interval: time.Hour, OutputDir: t.TempDir() + "/out-a"},
		{Enabled: true, Interval: 0, OutputDir: t.TempDir() + "/out-b"},
	} {
		s.StartSnapshotSweep(ctx, cfg) // returns synchronously; launches no goroutine
		_, err := os.Stat(cfg.OutputDir)
		assert.True(t, os.IsNotExist(err), "disabled sweep must not create %s", cfg.OutputDir)
	}
}

// TestRunSnapshotCycle_WritesAndPrunes — one cycle writes an archive per project
// and prunes old snapshots down to the retention count.
func TestRunSnapshotCycle_WritesAndPrunes(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "p1"))
	require.NoError(t, s.CreateProject("ns", "p2"))
	writeDrainP(t, s, "ns", "p1", "f.md", "one\n")
	writeDrainP(t, s, "ns", "p2", "f.md", "two\n")
	out := t.TempDir()

	// Pre-seed two OLD snapshots for p1 (well before "now"); the cycle's fresh
	// snapshot is newest, so keepCount=1 keeps only it and removes the two olds.
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	makeSnapshotFiles(t, out, "ns", "p1", []time.Time{old, old.Add(time.Hour)})

	written, pruned, err := s.RunSnapshotCycle(context.Background(), SnapshotSweepConfig{
		OutputDir: out, Scope: Scope{}, RetentionCount: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, written, "one archive per project")
	assert.Equal(t, 2, pruned, "the two old p1 snapshots are pruned")

	assert.Len(t, remainingNames(t, filepath.Join(out, "ns", "p1")), 1, "p1 keeps only the newest")
	assert.Len(t, remainingNames(t, filepath.Join(out, "ns", "p2")), 1, "p2 has its one fresh snapshot")
}

// TestRunSnapshotCycle_PerProjectFailureNonFatal — one project failing does not
// abort the cycle: the others still produce archives, and the error is returned.
func TestRunSnapshotCycle_PerProjectFailureNonFatal(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns1", "a"))
	require.NoError(t, s.CreateProject("ns2", "b"))
	writeDrainP(t, s, "ns1", "a", "f.md", "aaa\n")
	writeDrainP(t, s, "ns2", "b", "f.md", "bbb\n")
	out := t.TempDir()
	// Block ns2/b's output path with a pre-existing file.
	require.NoError(t, os.MkdirAll(filepath.Join(out, "ns2"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(out, "ns2", "b"), []byte("x"), 0o644))

	written, _, err := s.RunSnapshotCycle(context.Background(), SnapshotSweepConfig{
		OutputDir: out, Scope: Scope{}, RetentionCount: 0,
	})
	require.Error(t, err, "the blocked project's error is surfaced")
	assert.Equal(t, 1, written, "the healthy project still produced an archive")
	assert.Len(t, remainingNames(t, filepath.Join(out, "ns1", "a")), 1)
}
