package tests

import (
	"context"
	"strings"
	"testing"
	"time"

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
	// No drain: etag (content hash) and modified_at (filesystem mtime) are both
	// available immediately on write — neither waits for the background git commit.

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

// TestPhase4_ListFilesModifiedAt covers the 2026-05-30 modified_at directive:
// list_files always returns a modified_at map keyed by every entry, in RFC3339
// nanosecond format, and (when summaries are requested) each summary's
// modified_at mirrors the top-level value for the same path.
func TestPhase4_ListFilesModifiedAt(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "a.md",
		Content: "---\ntitle: A\n---\n# Alpha\n\nbody\n",
	})
	require.NoError(t, err)
	_, _, err = write(ctx, nil, tools.WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "sub/inner.md", Content: "x",
	})
	require.NoError(t, err)

	list := tools.ListFilesHandler(s)

	// Without summaries: modified_at is still always present.
	_, out, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj"})
	require.NoError(t, err)
	require.NotNil(t, out.ModifiedAt, "modified_at must always be present")
	require.NotEmpty(t, out.Files)
	for _, f := range out.Files {
		ts, ok := out.ModifiedAt[f]
		require.True(t, ok, "every file in files must have a modified_at key: %q", f)
		_, perr := time.Parse(time.RFC3339Nano, ts)
		assert.NoError(t, perr, "modified_at[%q]=%q must be RFC3339Nano-parseable", f, ts)
	}
	// Directory entries are included (trailing slash).
	require.Contains(t, out.ModifiedAt, "sub/")

	// With summaries: each summary's modified_at mirrors the top-level value.
	_, out2, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj", IncludeSummaries: true})
	require.NoError(t, err)
	require.Contains(t, out2.Summaries, "a.md")
	assert.Equal(t, out2.ModifiedAt["a.md"], out2.Summaries["a.md"].ModifiedAt,
		"summaries[a.md].modified_at must equal the top-level modified_at[a.md]")
	assert.NotEmpty(t, out2.Summaries["a.md"].ModifiedAt)
}

// TestPhase4_ReadSummaryModifiedAtMatchesListFiles covers the 2026-05-30
// read_summary.modified_at directive: read_summary.modified_at is the working-tree
// filesystem mtime (RFC3339 nanosecond UTC), available immediately on write, and
// byte-identical to both places list_files reports the same path's modified_at.
func TestPhase4_ReadSummaryModifiedAtMatchesListFiles(t *testing.T) {
	s := newToolStorage(t)
	ctx := context.Background()
	write := tools.WriteFileHandler(s)
	_, _, err := write(ctx, nil, tools.WriteFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "a.md",
		Content: "---\ntitle: A\n---\n# Alpha\n\nbody\n",
	})
	require.NoError(t, err)
	// Deliberately NO drain: mtime is on disk the moment the write returns, before
	// the background git commit lands. The previous git-derived modified_at would
	// have been empty here.

	summary := tools.ReadSummaryHandler(s)
	_, sout, err := summary(ctx, nil, tools.ReadSummaryInput{Namespace: "ns", ProjectName: "proj", Path: "a.md"})
	require.NoError(t, err)

	// Populated immediately, RFC3339 nanosecond precision, UTC ("Z" suffix).
	require.NotEmpty(t, sout.ModifiedAt, "modified_at must be populated without a git commit")
	_, perr := time.Parse(time.RFC3339Nano, sout.ModifiedAt)
	require.NoError(t, perr, "modified_at=%q must be RFC3339Nano-parseable", sout.ModifiedAt)
	assert.True(t, strings.HasSuffix(sout.ModifiedAt, "Z"), "modified_at must be UTC: %q", sout.ModifiedAt)

	// Byte-identical across all three places modified_at appears for the same path
	// at the same moment.
	list := tools.ListFilesHandler(s)
	_, lout, err := list(ctx, nil, tools.ListFilesInput{Namespace: "ns", ProjectName: "proj", IncludeSummaries: true})
	require.NoError(t, err)
	assert.Equal(t, lout.ModifiedAt["a.md"], sout.ModifiedAt,
		"read_summary.modified_at must equal list_files.modified_at[a.md]")
	assert.Equal(t, lout.Summaries["a.md"].ModifiedAt, sout.ModifiedAt,
		"read_summary.modified_at must equal list_files.summaries[a.md].modified_at")
}
