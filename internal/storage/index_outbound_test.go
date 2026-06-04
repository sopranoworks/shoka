package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I3 §3.1 — the write hook derives the markdown file's outbound internal links
// from the content it already carries (the same content I2 derives bigrams from),
// via scanOutboundLinks, so the reverse-link index is incremental-by-construction.
func TestIndexUpdate_WriteStoresOutboundLinks(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	body := "see [a](target.md) and [b](../up.md) and [ext](https://x.com/y.md)"
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "docs/ref.md", body, nil)
	require.NoError(t, err)

	rec, ok := idxRecord(t, s, "ns", "proj", "docs/ref.md")
	require.True(t, ok)
	// Resolved relative to docs/ref.md: target.md -> docs/target.md, ../up.md -> up.md;
	// the external URL is excluded.
	assert.Equal(t, []string{"docs/target.md", "up.md"}, rec.OutboundLinks)
}

// Outbound links are derived only for markdown files: a non-.md file with
// link-like text carries no outbound links, so Referrers (and thus fix_links)
// never treats a non-markdown file as a referrer — matching discoverReferrers'
// .md-only corpus.
func TestIndexUpdate_OutboundLinksMarkdownOnly(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "data.txt", "[a](target.md)", nil)
	require.NoError(t, err)

	rec, ok := idxRecord(t, s, "ns", "proj", "data.txt")
	require.True(t, ok)
	assert.Nil(t, rec.OutboundLinks, "a non-markdown file must carry no outbound links")
}

// I3 §3.1 — the wholesale rebuild derives outbound links from working-tree bytes
// (.md-only), so a stale/missing/corrupt index recovers the reverse-link map, not
// just etags and bigrams.
func TestIndexSweep_RebuildDerivesOutboundLinks(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "[a](target.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "note.txt", "[a](target.md)", nil)
	require.NoError(t, err)
	drain(t, s)

	// Force a wholesale rebuild from the working tree.
	s.reconcileIndex("ns", "proj")

	ix := openIndexRO(t, s, "ns", "proj")
	rec, found, _ := ix.GetRecord("ref.md")
	require.True(t, found)
	assert.Equal(t, []string{"target.md"}, rec.OutboundLinks, "rebuild must derive outbound links for markdown")
	txt, found, _ := ix.GetRecord("note.txt")
	require.True(t, found)
	assert.Nil(t, txt.OutboundLinks, "rebuild must not derive outbound links for non-markdown")
	refs, err := ix.Referrers("target.md")
	require.NoError(t, err)
	assert.Equal(t, []string{"ref.md"}, refs, "rebuilt reverse-link map answers who-links-to-P")
}

// The reverse-link index answers "who links to P" end to end through storage:
// after writing two referrers and a non-referrer, Referrers(target) is exactly
// the referrer set, sorted.
func TestReverseLink_ReferrersThroughStorage(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "[x](target.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "docs/b.md", "[y](../target.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "c.md", "[z](other.md)", nil)
	require.NoError(t, err)

	ix := s.indexForRead("ns", "proj")
	require.NotNil(t, ix)
	refs, err := ix.Referrers("target.md")
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "docs/b.md"}, refs)
}
