package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newNotifyStore builds storage with a real notification center injected and
// creates the project "ns/proj". It returns the storage and the center. The
// project.create event from CreateProject is left in the center; callers that
// only care about subsequent events should snapshot first or account for it.
func newNotifyStore(t *testing.T) (*FSGitStorage, *notify.Center) {
	t.Helper()
	nc := notify.NewCenter(100)
	s, _ := newStore(t, Options{NotifyCenter: nc})
	require.NoError(t, s.CreateProject("ns", "proj"))
	return s, nc
}

func TestNotify_WritePublishesOneEvent(t *testing.T) {
	nc := notify.NewCenter(100)
	s, _ := newStore(t, Options{NotifyCenter: nc})
	require.NoError(t, s.CreateProject("ns", "proj"))

	before := len(nc.Snapshot())
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "dir/a.md", "hello", nil)
	require.NoError(t, err)

	events := nc.Snapshot()
	require.Len(t, events, before+1, "exactly one new event for a successful write")
	e := events[len(events)-1]
	assert.Equal(t, "file.write", e.Kind)
	assert.Equal(t, "ns/proj", e.Target)
	assert.Equal(t, "dir/a.md", e.Path)
}

func TestNotify_DeletePublishesOneEvent(t *testing.T) {
	s, nc := newNotifyStore(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	before := len(nc.Snapshot())
	require.NoError(t, s.Delete(context.Background(), "sess", "ns", "proj", "a.md", nil))

	events := nc.Snapshot()
	require.Len(t, events, before+1, "exactly one new event for a successful delete")
	e := events[len(events)-1]
	assert.Equal(t, "file.delete", e.Kind)
	assert.Equal(t, "ns/proj", e.Target)
	assert.Equal(t, "a.md", e.Path)
}

func TestNotify_CreateProjectPublishesWithEmptyPath(t *testing.T) {
	nc := notify.NewCenter(100)
	s, _ := newStore(t, Options{NotifyCenter: nc})

	require.NoError(t, s.CreateProject("ns", "fresh"))
	events := nc.Snapshot()
	require.Len(t, events, 1)
	e := events[0]
	assert.Equal(t, "project.create", e.Kind)
	assert.Equal(t, "ns/fresh", e.Target)
	assert.Equal(t, "", e.Path, "project-level event carries empty path")

	// Re-creating an existing project (ErrRepositoryAlreadyExists early return)
	// must NOT publish a second event.
	require.NoError(t, s.CreateProject("ns", "fresh"))
	assert.Len(t, nc.Snapshot(), 1, "re-create of existing project must not publish")
}

func TestNotify_FailedWriteByConflictPublishesNothing(t *testing.T) {
	s, nc := newNotifyStore(t)
	etag, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v1", nil)
	require.NoError(t, err)

	before := nc.Snapshot()
	stale := contentSHA("not-the-current-etag")
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v2", &stale)
	require.Error(t, err, "stale if_match must fail")

	assert.Len(t, nc.Snapshot(), len(before), "failed (conflict) write must not publish")
	_ = etag
}

func TestNotify_FailedWriteByCorruptedStatePublishesNothing(t *testing.T) {
	s, nc := newNotifyStore(t)
	// GENUINE corruption: write a tracked file, then hand-edit it off the write
	// path so the working tree diverges from the catalog. This must be a real
	// divergence, not merely a stale in-memory mark — under D1 (B-25) a clean tree
	// with a stale corrupted mark is lazily rescanned and the write proceeds, so
	// only a true divergence keeps the corrupted refusal that this test pins.
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v1", nil)
	require.NoError(t, err)
	require.True(t, s.WaitForWAL(10*time.Second), "WAL must drain before hand-editing")
	require.NoError(t, os.WriteFile(filepath.Join(s.baseDir, "ns", "proj", "a.md"), []byte("hand-edited"), 0o644))
	s.setState("ns", "proj", StateCorrupted)

	before := nc.Snapshot()
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.ErrorIs(t, err, ErrProjectCorrupted, "a genuinely-corrupted project stays refused after the lazy rescan")

	assert.Len(t, nc.Snapshot(), len(before), "write refused by corrupted state must not publish")
}

func TestNotify_FailedDeleteByConflictPublishesNothing(t *testing.T) {
	s, nc := newNotifyStore(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v1", nil)
	require.NoError(t, err)

	before := nc.Snapshot()
	stale := contentSHA("wrong")
	err = s.Delete(context.Background(), "sess", "ns", "proj", "a.md", &stale)
	require.Error(t, err, "stale if_match must fail the delete")

	assert.Len(t, nc.Snapshot(), len(before), "failed (conflict) delete must not publish")
}

func TestNotify_NilCenterOperationsSucceed(t *testing.T) {
	// Storage with no notification center: every operation must still succeed
	// and never panic.
	s, _ := newStore(t, Options{NotifyCenter: nil})
	require.NoError(t, s.CreateProject("ns", "proj"))

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	require.NoError(t, s.Delete(context.Background(), "sess", "ns", "proj", "a.md", nil))
}
