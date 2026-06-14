package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage/wal"
	"github.com/sopranoworks/shoka/internal/storage/walworker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// D3 storage-side tests (B-38.2): the repo-absent classification in commitEntry and
// the end-to-end quarantine of a permanently-uncommittable WAL entry to lost+found.
// The walworker-level mechanics (process() returns, pool-not-saturated, backstop)
// live in internal/storage/walworker/quarantine_test.go.

// findLostFoundFile returns the deposited path under <base>/<ns>/.shoka-lostfound/
// <project>/ whose tail matches relPath, or "" if none.
func findLostFoundFile(t *testing.T, baseDir, ns, project, relPath string) string {
	t.Helper()
	root := filepath.Join(baseDir, ns, ".shoka-lostfound", project)
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(filepath.ToSlash(p), relPath) {
			found = p
		}
		return nil
	})
	return found
}

// TestCommitEntry_RepoAbsentReturnsPermanentSignal pins §4.1: commitEntry classifies
// a repo-absent project as PERMANENT (wrapping walworker.ErrCommitPermanent) so the
// walworker quarantines instead of looping — while keeping the underlying go-git
// cause in the chain for diagnostics and keeping the existing dangerous marking.
func TestCommitEntry_RepoAbsentReturnsPermanentSignal(t *testing.T) {
	s, _ := newStore(t, Options{})

	// No project created: "ghost/maintenance" has no git repo.
	err := s.commitEntry(context.Background(), wal.Entry{
		Namespace: "ghost", Project: "maintenance",
		Path: "directives/x.md", Op: "write", Content: []byte("payload"),
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, walworker.ErrCommitPermanent),
		"repo-absent must be classified permanent so the walworker quarantines it")
	assert.True(t, errors.Is(err, git.ErrRepositoryNotExists),
		"the underlying cause must stay in the chain for diagnostics")
	assert.Equal(t, StateDangerous, s.State("ghost", "maintenance"),
		"the project is still marked dangerous (its state is genuinely bad)")
}

// TestWALWorker_RepoAbsentEntryQuarantinedToLostFound is the end-to-end §4.2 path: a
// WAL entry for a repo-absent project drains, its content is deposited to lost+found
// at the original path (repo absent), the entry leaves the WAL, the project repo is
// NOT created, and the lostfound.quarantined NOTIFY fires. The entry is injected
// directly into the WAL because the B-37 write guard blocks a repo-less write at
// checkWritable — but a pre-existing / otherwise-permanent entry still reaches the
// worker, which is exactly the class D3 handles.
func TestWALWorker_RepoAbsentEntryQuarantinedToLostFound(t *testing.T) {
	center := notify.NewCenter(64)
	s, dir := newStore(t, Options{NotifyCenter: center})

	const ns, proj, path = "ghost", "maintenance", "directives/x.md"
	payload := []byte("orphan payload")

	_, err := s.wal.Append(wal.Entry{Namespace: ns, Project: proj, Path: path, Op: "write", Content: payload})
	require.NoError(t, err)
	s.pool.Notify()

	require.True(t, s.WaitForWAL(5*time.Second), "the uncommittable entry must leave the WAL (quarantined)")

	deposited := findLostFoundFile(t, dir, ns, proj, path)
	require.NotEmpty(t, deposited, "the entry's content must be deposited to lost+found")
	got, err := os.ReadFile(deposited)
	require.NoError(t, err)
	assert.Equal(t, payload, got, "the deposited content is the WAL entry's content")

	_, statErr := os.Stat(filepath.Join(dir, ns, proj, ".git"))
	assert.True(t, os.IsNotExist(statErr), "quarantine must not create the project repo")

	assert.Equal(t, int64(1), s.WorkerStats().QuarantinedTotal)
	assert.Eventually(t, func() bool {
		for _, ev := range center.Snapshot() {
			if ev.Kind == kindLostFoundQuarantined && ev.Target == ns+"/"+proj && ev.Path == path {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "lostfound.quarantined must be emitted for the quarantined entry")
}

// TestWALWorker_RepoAbsentEntryQuarantinedOnBootReplay pins §4.3: a pre-existing
// permanent entry (left from a previous run) is quarantined on its FIRST process()
// at boot — not looped — and does NOT replay on the next boot (it left the WAL).
func TestWALWorker_RepoAbsentEntryQuarantinedOnBootReplay(t *testing.T) {
	dir := t.TempDir()
	const ns, proj, path = "ghost", "maintenance", "directives/x.md"
	payload := []byte("pre-existing orphan")

	// Pre-seed the WAL with a repo-absent entry WITHOUT a running pool, simulating an
	// entry that survived a restart and can never commit.
	w, err := wal.Open(dir)
	require.NoError(t, err)
	_, err = w.Append(wal.Entry{Namespace: ns, Project: proj, Path: path, Op: "write", Content: payload})
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// First boot: NewPool drains the pre-existing entry and quarantines it at once.
	s1, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	require.True(t, s1.WaitForWAL(5*time.Second), "the pre-existing entry must quarantine on first boot")
	require.Eventually(t, func() bool { return s1.WorkerStats().QuarantinedTotal == 1 },
		2*time.Second, 10*time.Millisecond, "exactly one quarantine on first boot, no loop")
	require.NotEmpty(t, findLostFoundFile(t, dir, ns, proj, path), "content preserved on first boot")
	require.NoError(t, s1.Close())

	// Second boot over the SAME dir: the WAL is empty, so nothing replays.
	s2, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	assert.Equal(t, 0, s2.WALPending(), "no entry replays on the next boot")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(0), s2.WorkerStats().QuarantinedTotal, "nothing to re-quarantine on the next boot")
}
