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

// The B-37 phantom-project reproductions (committed, red→green). A write addressed
// to a project that was never created (no git repo behind it) used to be accepted:
// it half-created a project — a working-tree directory, a per-project catalog .db,
// and an un-committable WAL entry the worker then looped on forever, marking the
// phantom "dangerous" on every boot. Deleting the WAL entry did not help because
// startup catalog init re-discovered the leftover directory + .db. The fix guards
// BOTH paths; these two tests pin both. See
// shoka/maintenance/directives/2026-06-03-shoka-phantom-project-discovery-investigation.md.

// TestProjectGuard_WritePathRejectsRepolessProject is the §3 write-path
// reproduction: every mutation (write/delete/append/patch/move) on a project with
// no git repo must be refused with ErrProjectNotFound BEFORE any side-effect — no
// working-tree directory, no per-project .db, no pending WAL entry. This also pins
// that all five mutations funnel through the single checkWritable gate. Red before
// the §2.1 guard (the write succeeds and leaves a leftover + orphan WAL entry).
func TestProjectGuard_WritePathRejectsRepolessProject(t *testing.T) {
	s, dir := newStore(t, Options{})
	ctx := context.Background()
	// No CreateProject: "ghost/maintenance" has no .git — it is not a project.
	const ns, proj = "ghost", "maintenance"

	mutations := []struct {
		name string
		call func() error
	}{
		{"write", func() error { _, e := s.Write(ctx, "sess", ns, proj, "a.md", "x", nil); return e }},
		{"delete", func() error { return s.Delete(ctx, "sess", ns, proj, "a.md", nil) }},
		{"append", func() error { _, e := s.AppendToFile(ctx, "sess", ns, proj, "a.md", "x", "end", "", nil); return e }},
		{"patch", func() error { _, e := s.PatchFile(ctx, "sess", ns, proj, "a.md", "x", "y", nil); return e }},
		{"move", func() error { _, _, e := s.Move(ctx, "sess", ns, proj, "a.md", "b.md", nil); return e }},
	}
	for _, m := range mutations {
		assert.ErrorIs(t, m.call(), ErrProjectNotFound,
			"%s on a repo-less project must be rejected with ErrProjectNotFound", m.name)
	}

	// No side-effects may have escaped: a guard-less write half-creates all three.
	_, statErr := os.Stat(filepath.Join(dir, ns, proj))
	assert.Truef(t, os.IsNotExist(statErr), "no working-tree directory may be created (stat err=%v)", statErr)
	_, dbErr := os.Stat(filepath.Join(dir, ns, proj+".project.db"))
	assert.Truef(t, os.IsNotExist(dbErr), "no per-project catalog .db may be created (stat err=%v)", dbErr)
	assert.Equal(t, 0, s.WALPending(), "no WAL entry may be appended for a repo-less project")
}

// TestProjectGuard_CatalogInitSkipsGitlessLeftover is the §3 catalog-init
// reproduction: a base_dir containing a real git-backed project AND a leftover
// <ns>/<name>/ directory with no .git (plus a stray <name>.db, exactly what a
// pre-B-37 guard-less write left behind) must, after startup catalog init, leave
// the leftover UNREGISTERED — not given a state, not marked dangerous, not handed
// to the WAL worker. Red before the §2.2 guard (init re-adopts the leftover and
// marks it dangerous).
func TestProjectGuard_CatalogInitSkipsGitlessLeftover(t *testing.T) {
	dir := t.TempDir()

	// A real, git-backed project that must stay healthy.
	s1, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	require.NoError(t, s1.CreateProject("shoka", "maintenance"))
	_, err = s1.Write(context.Background(), "sess", "shoka", "maintenance", "a.md", "real", nil)
	require.NoError(t, err)
	require.True(t, s1.WaitForWAL(10*time.Second), "WAL must drain before close")
	require.NoError(t, s1.Close())

	// The leftover: a directory tree with content but no .git, plus a stray
	// per-project catalog .db — the half-project a guard-less write produced.
	leftover := filepath.Join(dir, "default", "maintenance", "directives")
	require.NoError(t, os.MkdirAll(leftover, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(leftover, "x.md"), []byte("orphan"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "default", "maintenance.project.db"), make([]byte, 32768), 0o600))

	s2 := freshStore(t, dir)
	s2.StartupInit(context.Background())
	// Await the non-blocking relocation goroutine before the test returns, so
	// t.TempDir() cleanup never races a mid-deposit lost+found dir (B-42).
	s2.relocWG.Wait()

	// The real project is registered and healthy.
	assert.Equal(t, StateHealthy, s2.State("shoka", "maintenance"))

	// The leftover is not a project: unregistered, with no dangerous phantom anywhere.
	states := s2.AllStates()
	_, registered := states["default/maintenance"]
	assert.False(t, registered, "a .git-less leftover directory must not be registered as a project")
	for key, st := range states {
		assert.NotEqualf(t, StateDangerous, st, "no project may be dangerous (phantom) after init: %s", key)
	}
}
