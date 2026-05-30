package tests

import (
	"context"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase5_ListFilesSince_FindsWrite(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "hi"})
	require.NoError(t, err)
	drainTool(t, s) // ListFilesSince is git-backed (async commit)

	since := tools.ListFilesSinceHandler(s)
	res, out, err := since(ctx, nil, tools.ListFilesSinceInput{Namespace: "ns", ProjectName: "proj", Since: before})
	require.NoError(t, err)
	require.Nil(t, res)

	found := false
	for _, c := range out.Changes {
		if c.Path == "a.md" {
			found = true
			assert.Equal(t, "added", c.Kind)
		}
	}
	assert.True(t, found, "expected a.md in changes since %s: %+v", before, out.Changes)
}

func TestPhase5_ListFilesSince_EmptyProjectIsGraceful(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	since := tools.ListFilesSinceHandler(s)
	res, out, err := since(ctx, nil, tools.ListFilesSinceInput{
		Namespace: "ns", ProjectName: "proj",
		Since: time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)
	require.Nil(t, res)
	assert.Empty(t, out.Changes)
}

func TestPhase5_SearchFiles_Content(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "doc.md", Content: "please find me here"})
	require.NoError(t, err)

	search := tools.SearchFilesHandler(s)
	_, out, err := search(ctx, nil, tools.SearchFilesInput{Namespace: "ns", ProjectName: "proj", Query: "find me", SearchIn: "content"})
	require.NoError(t, err)
	require.Len(t, out.Matches, 1)
	assert.Equal(t, "doc.md", out.Matches[0].Path)
	assert.Contains(t, out.Matches[0].Snippet, "find me")
}

func TestPhase5_SearchFiles_NoMatchIsGraceful(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "a.md", Content: "content"})
	require.NoError(t, err)

	search := tools.SearchFilesHandler(s)
	res, out, err := search(ctx, nil, tools.SearchFilesInput{Namespace: "ns", ProjectName: "proj", Query: "zzzzz", SearchIn: "both"})
	require.NoError(t, err)
	require.Nil(t, res)
	assert.Empty(t, out.Matches)
}

func TestPhase5_GetHistorySince_HashExclusive(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	for _, v := range []string{"v1", "v2", "v3"} {
		_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "h.txt", Content: v})
		require.NoError(t, err)
	}
	drainTool(t, s) // GetHistory is git-backed (async commit)

	history := tools.GetHistoryHandler(s)
	_, all, err := history(ctx, nil, tools.GetHistoryInput{Namespace: "ns", ProjectName: "proj", Path: "h.txt"})
	require.NoError(t, err)
	require.Len(t, all.History, 3)

	oldest := all.History[2].Hash // history is newest-first
	_, since, err := history(ctx, nil, tools.GetHistoryInput{Namespace: "ns", ProjectName: "proj", Path: "h.txt", Since: oldest})
	require.NoError(t, err)
	assert.Len(t, since.History, 2, "since the oldest commit (exclusive) should return the 2 newer commits")
}
