package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/shoka/mcp-server/internal/markdown"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhase4_ReadSummary(t *testing.T) {
	s := newToolStorage(t) // defined in phase3_test.go
	ctx := context.Background()
	write := tools.WriteFileHandler(s)

	content := "---\ntitle: Doc One\nsummary: short\nstatus: active\n---\n# Heading One\n\nThe opening paragraph.\n\nMore body that should not appear in the excerpt fully.\n"
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "doc.md", Content: content})
	require.NoError(t, err)
	drainTool(t, s) // version/modified_at come from git history (async commit)

	summary := tools.ReadSummaryHandler(s)
	res, out, err := summary(ctx, nil, tools.ReadSummaryInput{Namespace: "ns", ProjectName: "proj", Path: "doc.md"})
	require.NoError(t, err)
	require.Nil(t, res)

	assert.Equal(t, "Doc One", out.Frontmatter["title"])
	assert.Equal(t, "active", out.Frontmatter["status"])
	assert.Equal(t, "Heading One", out.Heading)
	assert.Equal(t, "The opening paragraph.", out.Excerpt)
	assert.Equal(t, len(content), out.Size)
	assert.NotEmpty(t, out.ETag)
	assert.NotEmpty(t, out.ModifiedAt)
	// The full body must not leak through the excerpt.
	assert.NotContains(t, out.Excerpt, "More body")
}

func TestPhase4_ReadSummary_CapsHugeBody(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)

	huge := "# H\n\n" + strings.Repeat("word ", 6000)
	_, _, err := write(ctx, nil, tools.WriteFileInput{Namespace: "ns", ProjectName: "proj", Path: "huge.md", Content: huge})
	require.NoError(t, err)

	summary := tools.ReadSummaryHandler(s)
	_, out, err := summary(ctx, nil, tools.ReadSummaryInput{Namespace: "ns", ProjectName: "proj", Path: "huge.md"})
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(out.Excerpt)), markdown.MaxExcerptRunes)
}

func TestPhase4_ListFilesIncludeSummaries(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "a.md",
		Content: "---\ntitle: A\n---\n# Alpha\n\nbody\n",
	})
	require.NoError(t, err)

	list := tools.ListFilesHandler(s)
	_, out, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj", IncludeSummaries: true})
	require.NoError(t, err)
	require.NotNil(t, out.Summaries)
	require.Contains(t, out.Summaries, "a.md")
	assert.Equal(t, "A", out.Summaries["a.md"].Frontmatter["title"])
	assert.Equal(t, "Alpha", out.Summaries["a.md"].Heading)
}
