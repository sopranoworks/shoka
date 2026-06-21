package librariansrc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func newStore(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	s, err := storage.NewFSGitStorage(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSGitStorage: %v", err)
	}
	return s
}

func write(t *testing.T, s *storage.FSGitStorage, ns, proj, path, content string) {
	t.Helper()
	if _, err := s.WriteFileVersioned(ns, proj, path, content, ""); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// project creates ns/proj and registers teardown of the storage worker pool.
func project(t *testing.T, s *storage.FSGitStorage, ns, proj string) {
	t.Helper()
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject(ns, proj); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
}

// TestCorpus_GranularityFix is the substantive proof: a question answerable from
// a LARGE single file (~hundreds of KB) reads only a BOUNDED span seeded from the
// search hit's offset — never the whole file. This is the B-73 point (design
// report §1.3): a file-level hit must not pour the whole file into the
// librarian's context.
func TestCorpus_GranularityFix(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "backlog"
	project(t, s, ns, proj)

	// Build a large single file with a unique needle on a known line.
	const needleLine = 4000
	var b strings.Builder
	for i := 0; i < 8000; i++ {
		if i == needleLine {
			b.WriteString("NEEDLE-ITEM B-999: the widget retry budget is 7\n")
		} else {
			fmt.Fprintf(&b, "- B-%04d: routine backlog line padding to grow the file\n", i)
		}
	}
	big := b.String()
	if len(big) < 200_000 {
		t.Fatalf("fixture too small (%d bytes); need a genuinely large file", len(big))
	}
	write(t, s, ns, proj, "backlog.md", big)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain")
	}

	corpus := NewCorpus(s, ns, proj)
	ctx := context.Background()

	// Search carries the passage offset (the match line).
	hits, err := corpus.Search(ctx, "NEEDLE-ITEM B-999", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Path != "backlog.md" {
		t.Fatalf("hits = %+v, want one for backlog.md", hits)
	}
	if hits[0].Offset != needleLine {
		t.Errorf("hit offset = %d, want %d", hits[0].Offset, needleLine)
	}

	// A read seeded at the offset returns a BOUNDED span, NOT the whole file.
	span, err := corpus.Read(ctx, "backlog.md", hits[0].Offset, 1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(span), "retry budget is 7") {
		t.Errorf("ranged read missed the needle: %q", span)
	}
	if len(span) > 200 {
		t.Errorf("ranged read returned %d bytes; expected a bounded span, not the whole %d-byte file", len(span), len(big))
	}
	// Explicit: the span is a tiny fraction of the file.
	if len(span)*10 >= len(big) {
		t.Errorf("ranged read (%d bytes) is not bounded vs the file (%d bytes)", len(span), len(big))
	}
}

// TestCorpus_ListAndRead covers the List mapping (leaf names, dirs trailing "/")
// and full-file Read.
func TestCorpus_ListAndRead(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "docs"
	project(t, s, ns, proj)
	write(t, s, ns, proj, "a.md", "alpha\nbeta\ngamma")
	write(t, s, ns, proj, "guide/b.md", "in a subdir")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain")
	}
	corpus := NewCorpus(s, ns, proj)
	ctx := context.Background()

	entries, err := corpus.List(ctx, ".")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.IsDir
	}
	if isDir, ok := got["a.md"]; !ok || isDir {
		t.Errorf("a.md missing or marked dir: %+v", entries)
	}
	if isDir, ok := got["guide"]; !ok || !isDir {
		t.Errorf("guide/ missing or not marked dir: %+v", entries)
	}

	full, err := corpus.Read(ctx, "a.md", 0, 0)
	if err != nil || string(full) != "alpha\nbeta\ngamma" {
		t.Errorf("full read = (%q, %v), want all lines", full, err)
	}
	ranged, err := corpus.Read(ctx, "a.md", 1, 1)
	if err != nil || string(ranged) != "beta" {
		t.Errorf("ranged read = (%q, %v), want 'beta'", ranged, err)
	}
}

// scriptedClient is a minimal fake llm.Client for driving the full librarian
// over the Shoka corpus.
type scriptedClient struct {
	replies []llm.Message
	step    int
	seen    []llm.CreateMessageParams
}

func (c *scriptedClient) CreateMessage(_ context.Context, p llm.CreateMessageParams) (llm.Message, error) {
	c.seen = append(c.seen, p)
	if c.step >= len(c.replies) {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "done"}}}, nil
	}
	r := c.replies[c.step]
	c.step++
	return r, nil
}

// TestCorpus_IgnoredExcludedThroughLibrarian proves that, wired through the
// librarian with Shoka's ignore patterns, a search never surfaces an ignored
// file even though it contains the query — the guard drops the hit.
func TestCorpus_IgnoredExcludedThroughLibrarian(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "mixed"
	project(t, s, ns, proj)
	write(t, s, ns, proj, "public.md", "the token of interest is FINDME")
	write(t, s, ns, proj, "secrets.shoka", "FINDME lives in an ignored file too")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain")
	}

	root, err := s.ProjectPath(ns, proj)
	if err != nil {
		t.Fatalf("ProjectPath: %v", err)
	}

	client := &scriptedClient{replies: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{{
			Type: llm.BlockToolUse, ID: "s1", Name: "search",
			Input: mustJSON(map[string]any{"query": "FINDME"}),
		}}},
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "answered"}}},
	}}

	lib := librarian.New(client, 4)
	_, err = lib.Ask(context.Background(), librarian.Request{
		Question:       "where is FINDME?",
		Root:           root,
		IgnorePatterns: []string{"*.shoka"},
		Corpus:         NewCorpus(s, ns, proj),
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Inspect the tool_result fed back: it must mention public.md but NOT the
	// ignored *.shoka file.
	var searchResult string
	for _, p := range client.seen {
		for _, m := range p.Messages {
			for _, blk := range m.Content {
				if blk.Type == llm.BlockToolResult {
					searchResult = blk.Content
				}
			}
		}
	}
	if !strings.Contains(searchResult, "public.md") {
		t.Errorf("search result missing the in-policy hit: %q", searchResult)
	}
	if strings.Contains(searchResult, ".shoka") {
		t.Errorf("search result surfaced an ignored file: %q", searchResult)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
