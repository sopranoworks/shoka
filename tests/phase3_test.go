package tests

import (
	"context"
	"os"
	"testing"

	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newToolStorage(t *testing.T) storage.StorageService {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-phase3-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorage(dir)
	require.NoError(t, err)
	require.NoError(t, s.CreateProject("ns", "proj"))
	return s
}

// Two clients read the same version, both attempt a write with that version;
// the first wins and the second receives a conflict carrying the current hash.
func TestPhase3_OptimisticLockingConflict(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	read := tools.ReadFileHandler(s)

	_, w1, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v1"})
	require.NoError(t, err)
	require.NotEmpty(t, w1.Version)

	_, rA, err := read(ctx, nil, tools.ReadFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md"})
	require.NoError(t, err)
	_, rB, err := read(ctx, nil, tools.ReadFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md"})
	require.NoError(t, err)
	require.Equal(t, rA.Version, rB.Version)
	require.NotEmpty(t, rA.Version)
	v := rA.Version

	// Client A wins.
	resA, wA, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v2-A", ExpectedVersion: v})
	require.NoError(t, err)
	require.Nil(t, resA)
	require.NotEqual(t, v, wA.Version)

	// Client B conflicts on the now-stale version.
	resB, wB, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "v2-B", ExpectedVersion: v})
	require.NoError(t, err)
	require.NotNil(t, resB)
	assert.True(t, resB.IsError)
	assert.True(t, wB.Conflict)
	assert.Equal(t, wA.Version, wB.CurrentVersion)

	content, err := s.ReadFile("ns", "proj", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "v2-A", content)
}

func TestPhase3_ListFilesIncludeVersions(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "one.md", Content: "1"})
	require.NoError(t, err)
	_, _, err = write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "two.md", Content: "2"})
	require.NoError(t, err)

	list := tools.ListFilesHandler(s)
	_, out, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj", IncludeVersions: true})
	require.NoError(t, err)
	require.NotNil(t, out.Versions)
	assert.NotEmpty(t, out.Versions["one.md"])
	assert.NotEmpty(t, out.Versions["two.md"])
}
