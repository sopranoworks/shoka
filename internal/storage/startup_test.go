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

// seedTwoProjects creates ns/p1 and ns/p2 with one file each, drains them into
// git, and closes the storage — leaving a base dir on disk that a fresh storage
// can start over (catalog .db files present). Returns the base dir.
func seedTwoProjects(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	require.NoError(t, s.CreateProject("ns", "p1"))
	require.NoError(t, s.CreateProject("ns", "p2"))
	_, err = s.Write(context.Background(), "sess", "ns", "p1", "a.md", "alpha", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "p2", "b.md", "bravo", nil)
	require.NoError(t, err)
	require.True(t, s.WaitForWAL(10*time.Second), "WAL must drain before close")
	require.NoError(t, s.Close())
	return dir
}

func freshStore(t *testing.T, dir string) *FSGitStorage {
	t.Helper()
	s, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStartup_AllDBsPresent(t *testing.T) {
	dir := seedTwoProjects(t)
	s := freshStore(t, dir)

	s.StartupInit(context.Background())

	assert.Equal(t, StateHealthy, s.State("ns", "p1"))
	assert.Equal(t, StateHealthy, s.State("ns", "p2"))
	for _, p := range []string{"p1", "p2"} {
		_, err := os.Stat(filepath.Join(dir, "ns", p+".project.db"))
		assert.NoError(t, err, "catalog db must exist for %s", p)
	}
}

func TestStartup_OneDBMissing(t *testing.T) {
	dir := seedTwoProjects(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "ns", "p2.project.db")))

	s := freshStore(t, dir)
	s.StartupInit(context.Background())

	assert.Equal(t, StateHealthy, s.State("ns", "p1"))
	assert.Equal(t, StateHealthy, s.State("ns", "p2"), "missing catalog must be rebuilt to healthy")
	assert.Equal(t, int64(1), s.catRebuildMissing.Load(), "one rebuild for the missing catalog")
	// The rebuilt catalog must contain the committed file.
	files, _, err := s.ListFiles("ns", "p2", "")
	require.NoError(t, err)
	assert.Contains(t, files, "b.md")
}

func TestStartup_OneDBCorrupted(t *testing.T) {
	dir := seedTwoProjects(t)
	// Replace p1's catalog with arbitrary garbage.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "p1.project.db"), make([]byte, 16384), 0o600))

	s := freshStore(t, dir)
	s.StartupInit(context.Background())

	assert.Equal(t, StateHealthy, s.State("ns", "p1"), "corrupt catalog must be rebuilt to healthy")
	assert.Equal(t, StateHealthy, s.State("ns", "p2"))
	assert.Equal(t, int64(1), s.catRebuildCorrupt.Load(), "one rebuild for the corrupt catalog")
	files, _, err := s.ListFiles("ns", "p1", "")
	require.NoError(t, err)
	assert.Contains(t, files, "a.md")
}

// TestStartup_GitlessDirectoryNotRegistered pins the B-37 §2.2 contract: a
// directory with no .git is not a project. Catalog init must NOT register it (it
// formerly marked such a directory dangerous and the WAL worker looped on it
// forever — the phantom). This supersedes the old "rebuild failure marks
// dangerous" expectation: a .git-less directory is leftover, surfaced via the
// discovery warning (and routed to lost+found by the B-38 follow-up), never a
// registered dangerous phantom. (A genuinely broken project that still HAS a .git
// is a different, still-dangerous case.)
func TestStartup_GitlessDirectoryNotRegistered(t *testing.T) {
	dir := seedTwoProjects(t)
	// Make p2 a .git-less leftover (no repo, stray working tree + catalog removed).
	require.NoError(t, os.Remove(filepath.Join(dir, "ns", "p2.project.db")))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "ns", "p2", ".git")))

	s := freshStore(t, dir)
	s.StartupInit(context.Background())
	// Await the non-blocking relocation goroutine before the test returns, so
	// t.TempDir() cleanup never races a mid-deposit lost+found dir (B-42).
	s.relocWG.Wait()

	assert.Equal(t, StateHealthy, s.State("ns", "p1"), "the real project is unaffected")
	_, registered := s.AllStates()["ns/p2"]
	assert.False(t, registered, "a .git-less directory must not be registered as a project")
	for key, st := range s.AllStates() {
		assert.NotEqualf(t, StateDangerous, st, "no project may be dangerous after init: %s", key)
	}
}

// TestStartup_GateComputesEveryProjectState asserts the gate's contract: when
// StartupInit returns, every discovered project has a computed state (so the
// listeners main() starts afterward never observe a project mid-initialisation).
func TestStartup_GateComputesEveryProjectState(t *testing.T) {
	dir := seedTwoProjects(t)
	s := freshStore(t, dir)

	s.StartupInit(context.Background())

	states := s.AllStates()
	require.Len(t, states, 2, "every project must have a state after the gate returns")
	for _, key := range []string{"ns/p1", "ns/p2"} {
		_, ok := states[key]
		assert.True(t, ok, "missing state for %s", key)
	}
}

// TestStartup_MigratesLegacyCatalogDB: a legacy <project>.db file (without .project
// suffix) is automatically renamed to <project>.project.db at startup.
func TestStartup_MigratesLegacyCatalogDB(t *testing.T) {
	dir := seedTwoProjects(t)

	// Simulate the pre-migration state: rename .project.db back to .db.
	for _, p := range []string{"p1", "p2"} {
		newPath := filepath.Join(dir, "ns", p+".project.db")
		oldPath := filepath.Join(dir, "ns", p+".db")
		require.NoError(t, os.Rename(newPath, oldPath))
	}

	s := freshStore(t, dir)
	s.StartupInit(context.Background())

	assert.Equal(t, StateHealthy, s.State("ns", "p1"))
	assert.Equal(t, StateHealthy, s.State("ns", "p2"))

	// The new .project.db files must exist.
	for _, p := range []string{"p1", "p2"} {
		_, err := os.Stat(filepath.Join(dir, "ns", p+".project.db"))
		assert.NoError(t, err, "migrated catalog must exist for %s", p)
	}
	// The old .db files must be gone.
	for _, p := range []string{"p1", "p2"} {
		_, err := os.Stat(filepath.Join(dir, "ns", p+".db"))
		assert.True(t, os.IsNotExist(err), "legacy catalog must be removed for %s", p)
	}
}
