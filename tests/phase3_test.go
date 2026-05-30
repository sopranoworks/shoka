package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newToolStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-phase3-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorage(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) // runs before RemoveAll: stops the worker pool
	require.NoError(t, s.CreateProject("ns", "proj"))
	return s
}

// drainTool waits for the background commit worker to flush the WAL to git, so
// git-derived assertions (history, ListFilesSince, read_summary version,
// webhooks) are observable. Commits are asynchronous in the redesign.
func drainTool(t *testing.T, s *storage.FSGitStorage) {
	t.Helper()
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain within 10s (pending=%d)", s.WALPending())
	}
}

// Two clients read the same etag, both attempt a write with that etag as
// if_match; the first wins and the second receives a conflict carrying the
// current etag.
func TestPhase3_OptimisticLockingConflict(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	read := tools.ReadFileHandler(s)

	_, w1, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v1"})
	require.NoError(t, err)
	require.NotEmpty(t, w1.ETag)

	_, rA, err := read(ctx, nil, tools.ReadFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md"})
	require.NoError(t, err)
	_, rB, err := read(ctx, nil, tools.ReadFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md"})
	require.NoError(t, err)
	require.Equal(t, rA.ETag, rB.ETag)
	require.NotEmpty(t, rA.ETag)
	v := rA.ETag

	// Client A wins.
	resA, wA, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v2-A", IfMatch: &v})
	require.NoError(t, err)
	require.Nil(t, resA)
	require.NotEqual(t, v, wA.ETag)

	// Client B conflicts on the now-stale etag.
	resB, wB, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v2-B", IfMatch: &v})
	require.NoError(t, err)
	require.NotNil(t, resB)
	assert.True(t, resB.IsError)
	assert.True(t, wB.Conflict)
	assert.Equal(t, wA.ETag, wB.CurrentETag)

	content, err := s.ReadFile("ns", "proj", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "v2-A", content)
}

func TestPhase3_ListFilesSummariesIncludeETag(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, w1, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "one.md", Content: "1"})
	require.NoError(t, err)
	_, w2, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "two.md", Content: "2"})
	require.NoError(t, err)

	list := tools.ListFilesHandler(s)
	_, out, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj", IncludeSummaries: true})
	require.NoError(t, err)
	require.NotNil(t, out.Summaries)
	assert.Equal(t, w1.ETag, out.Summaries["one.md"].ETag)
	assert.Equal(t, w2.ETag, out.Summaries["two.md"].ETag)
}
