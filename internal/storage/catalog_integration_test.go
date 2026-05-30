package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage/catalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCatalogStore makes a storage with a notification center and a created
// ns/proj project (so its catalog exists), returning the store and its notify
// center for event assertions.
func newCatalogStore(t *testing.T) (*FSGitStorage, *notify.Center) {
	t.Helper()
	nc := notify.NewCenter(256)
	s, _ := newStore(t, Options{NotifyCenter: nc})
	require.NoError(t, s.CreateProject("ns", "proj"))
	return s, nc
}

func TestCatalog_WriteInsertsEntry(t *testing.T) {
	s, _ := newCatalogStore(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "directives/a.md", "alpha", nil)
	require.NoError(t, err)

	c, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	entry, ok, err := c.GetFile("directives/a.md")
	require.NoError(t, err)
	require.True(t, ok, "catalog must contain the written file")
	assert.Equal(t, contentSHA("alpha"), entry.Etag)
	assert.Equal(t, int64(len("alpha")), entry.Size)
}

func TestCatalog_DeleteRemovesEntry(t *testing.T) {
	s, _ := newCatalogStore(t)
	ctx := context.Background()
	_, err := s.Write(ctx, "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, "sess", "ns", "proj", "a.md", nil))

	c, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	has, err := c.HasFile("a.md")
	require.NoError(t, err)
	assert.False(t, has, "catalog must not contain the deleted file")
}

func TestCatalog_ListFilesReadsFromCatalog(t *testing.T) {
	s, _ := newCatalogStore(t)
	ctx := context.Background()
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		_, err := s.Write(ctx, "sess", "ns", "proj", p, "x", nil)
		require.NoError(t, err)
	}
	files, _, err := s.ListFiles("ns", "proj", "")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.md", "b.md", "c.md"}, files)
}

func TestCatalog_ListFilesExcludesWorkingTreeNoise(t *testing.T) {
	s, _ := newCatalogStore(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "real.md", "x", nil)
	require.NoError(t, err)

	// Place noise directly in the working tree, bypassing Shoka entirely.
	projDir := filepath.Join(s.baseDir, "ns", "proj")
	require.NoError(t, os.WriteFile(filepath.Join(projDir, ".DS_Store"), []byte("junk"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(projDir, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projDir, ".claude", "settings.json"), []byte("{}"), 0o644))

	files, _, err := s.ListFiles("ns", "proj", "")
	require.NoError(t, err)
	assert.Contains(t, files, "real.md")
	assert.NotContains(t, files, ".DS_Store")
	assert.NotContains(t, files, ".claude/")
}

func TestCatalog_ReadFileAbsentPathIsCleanNotFound(t *testing.T) {
	s, _ := newCatalogStore(t)
	before := s.catInvariantViolations.Load()

	_, _, err := s.ReadFileWithETag("ns", "proj", "never-existed.md")
	require.Error(t, err, "absent path must be not-found")

	assert.Equal(t, before, s.catInvariantViolations.Load(),
		"a clean not-found (catalog also lacks the path) must not increment the invariant-violation metric")
}

func TestCatalog_ReadFileInvariantViolation(t *testing.T) {
	s, nc := newCatalogStore(t)
	ctx := context.Background()
	_, err := s.Write(ctx, "sess", "ns", "proj", "a.md", "alpha", nil)
	require.NoError(t, err)

	// Remove the working-tree file behind the catalog's back: the catalog still
	// claims a.md exists, the working tree no longer has it → invariant violation.
	require.NoError(t, os.Remove(filepath.Join(s.baseDir, "ns", "proj", "a.md")))

	before := s.catInvariantViolations.Load()
	_, _, err = s.ReadFileWithETag("ns", "proj", "a.md")
	require.Error(t, err, "read of a missing working-tree file must be not-found")

	assert.Equal(t, before+1, s.catInvariantViolations.Load(),
		"invariant-violation metric must increment")

	// The notify event must have been published.
	found := false
	for _, ev := range nc.Snapshot() {
		if ev.Kind == "catalog.invariant_violation" && ev.Target == "ns/proj" && ev.Path == "a.md" {
			found = true
		}
	}
	assert.True(t, found, "a catalog.invariant_violation event must be published")
}

func TestCatalog_CreateProjectCreatesCatalog(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("myns", "myproj"))

	dbPath := filepath.Join(s.baseDir, "myns", "myproj.db")
	_, statErr := os.Stat(dbPath)
	require.NoError(t, statErr, "<project>.db must exist after create_project")

	// catalogFor already registered the handle; close it via the store so the
	// file is not held, then re-open to assert meta.
	require.NoError(t, s.Close())
	c, err := catalog.Open(dbPath)
	require.NoError(t, err)
	defer c.Close()
	ns, _ := c.Meta(catalog.MetaNamespace)
	proj, _ := c.Meta(catalog.MetaProjectName)
	ver, _ := c.Meta(catalog.MetaSchemaVersion)
	assert.Equal(t, "myns", ns)
	assert.Equal(t, "myproj", proj)
	assert.Equal(t, catalog.CurrentSchemaVersion, ver)
}

func TestCatalog_WriteCatalogFailureIsNonFatal(t *testing.T) {
	s, _ := newCatalogStore(t)
	ctx := context.Background()

	// Close the project's catalog out from under the write path; the handle stays
	// registered, so catalogPut will reach a closed DB and PutFile will error.
	c, err := s.catalogFor("ns", "proj")
	require.NoError(t, err)
	require.NoError(t, c.Close())

	before := s.catUpdateFailedWrite.Load()
	etag, werr := s.Write(ctx, "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, werr, "write must still succeed (working tree + WAL succeeded)")
	assert.NotEmpty(t, etag)
	assert.Equal(t, before+1, s.catUpdateFailedWrite.Load(),
		"catalog update failure must increment the counter")

	// The working tree really has the file (the write was not rolled back).
	_, statErr := os.Stat(filepath.Join(s.baseDir, "ns", "proj", "a.md"))
	require.NoError(t, statErr)
}
