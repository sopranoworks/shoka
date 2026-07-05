package librariansrc

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeEmbedder returns a fixed vector per text content, allowing tests to
// control which chunks are "relevant" (high cosine similarity with the query).
type fakeEmbedder struct {
	vectors map[string][]float64
}

func (f *fakeEmbedder) EmbedText(_ context.Context, text string) ([]float64, error) {
	if v, ok := f.vectors[text]; ok {
		return v, nil
	}
	return []float64{0, 0, 0}, nil
}

func TestSplitChunks_Basic(t *testing.T) {
	content := "paragraph one\n\nparagraph two\n\nparagraph three"
	chunks := splitChunks(content, 0, 10000)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	if chunks[0].content != "paragraph one" {
		t.Errorf("chunk[0] = %q", chunks[0].content)
	}
	if chunks[1].content != "paragraph two" {
		t.Errorf("chunk[1] = %q", chunks[1].content)
	}
	if chunks[2].content != "paragraph three" {
		t.Errorf("chunk[2] = %q", chunks[2].content)
	}
}

func TestSplitChunks_Frontmatter(t *testing.T) {
	content := "---\ntitle: test\ntags: [a, b]\n---\n\nbody paragraph one\n\nbody paragraph two"
	chunks := splitChunks(content, 0, 10000)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want >=2", len(chunks))
	}
	if !chunks[0].isFrontmatter {
		t.Error("first chunk should be frontmatter")
	}
	if !strings.Contains(chunks[0].content, "title: test") {
		t.Errorf("frontmatter chunk missing content: %q", chunks[0].content)
	}
	for _, c := range chunks[1:] {
		if c.isFrontmatter {
			t.Error("non-first chunk marked as frontmatter")
		}
	}
}

func TestSplitChunks_MergeSmall(t *testing.T) {
	content := "hi\n\nworld\n\nthis is a longer paragraph that exceeds fifty characters easily enough"
	chunks := splitChunks(content, 50, 10000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 merged chunk, got %d", len(chunks))
	}
	if !strings.Contains(chunks[0].content, "hi") || !strings.Contains(chunks[0].content, "world") {
		t.Errorf("small chunks not merged: %q", chunks[0].content)
	}
}

func TestSplitChunks_SplitLarge(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, strings.Repeat("x", 40))
	}
	content := strings.Join(lines, "\n")
	chunks := splitChunks(content, 0, 100)
	if len(chunks) < 2 {
		t.Fatalf("large chunk not split: got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len(c.content) > 120 {
			t.Errorf("chunk too large after split: %d chars", len(c.content))
		}
	}
}

func TestChunkFilter_EmbedderAvailable_FiltersIrrelevant(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf"
	project(t, s, ns, proj)

	chunk1 := "relevant content about Go channels and their usage in concurrent programs"
	chunk2 := "irrelevant content about cooking pasta with tomato sauce and fresh basil"
	chunk3 := "more relevant content about goroutines and concurrency patterns in modern Go"
	content := "---\ntitle: test\n---\n\n" + chunk1 + "\n\n" + chunk2 + "\n\n" + chunk3
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	query := "Go concurrency patterns"
	embedder := &fakeEmbedder{vectors: map[string][]float64{
		query:  {1, 0, 0},
		chunk1: {0.9, 0.1, 0},
		chunk2: {0, 0, 1},
		chunk3: {0.8, 0.2, 0},
	}}

	corpus := NewCorpus(s, ns, proj).
		WithChunkFilter(embedder, query).
		WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	result := string(data)

	if !strings.Contains(result, "Go channels") {
		t.Error("filtered out relevant chunk about Go channels")
	}
	if !strings.Contains(result, "goroutines and concurrency") {
		t.Error("filtered out relevant chunk about goroutines")
	}
	if strings.Contains(result, "cooking pasta") {
		t.Error("irrelevant chunk about cooking pasta was not filtered")
	}
}

func TestChunkFilter_EmbedderNotAvailable_FullContent(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf2"
	project(t, s, ns, proj)

	content := "line one\n\nline two\n\nline three"
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	corpus := NewCorpus(s, ns, proj).WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != content {
		t.Errorf("without embedder, Read should return full content\ngot:  %q\nwant: %q", data, content)
	}
}

func TestChunkFilter_NoneAboveThreshold_FrontmatterPlusFirst(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf3"
	project(t, s, ns, proj)

	content := "---\ntitle: notes\n---\n\nfirst body paragraph\n\nsecond body paragraph"
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	query := "quantum physics"
	embedder := &fakeEmbedder{vectors: map[string][]float64{
		query: {1, 0, 0},
	}}

	corpus := NewCorpus(s, ns, proj).
		WithChunkFilter(embedder, query).
		WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	result := string(data)

	if !strings.Contains(result, "title: notes") {
		t.Error("frontmatter missing when no chunks pass threshold")
	}
	if !strings.Contains(result, "first body paragraph") {
		t.Error("first body chunk missing when no chunks pass threshold")
	}
}

func TestChunkFilter_FrontmatterAlwaysIncluded(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf4"
	project(t, s, ns, proj)

	content := "---\ntitle: important metadata\ntags: [api]\n---\n\nrelevant API documentation\n\nirrelevant filler text"
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	query := "API docs"
	embedder := &fakeEmbedder{vectors: map[string][]float64{
		query:                        {1, 0, 0},
		"relevant API documentation": {0.9, 0.1, 0},
		"irrelevant filler text":     {0, 0, 1},
	}}

	corpus := NewCorpus(s, ns, proj).
		WithChunkFilter(embedder, query).
		WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	result := string(data)

	if !strings.Contains(result, "title: important metadata") {
		t.Error("frontmatter not included in filtered output")
	}
	if !strings.Contains(result, "tags: [api]") {
		t.Error("frontmatter tags not included")
	}
}

func TestChunkFilter_PositionMarkers(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf5"
	project(t, s, ns, proj)

	chunk1 := "relevant chunk here with enough content to exceed the minimum chunk size threshold"
	chunk2 := "irrelevant middle section with padding text that is also long enough to stand alone"
	chunk3 := "another relevant chunk at end with sufficient content to avoid merging with others"
	content := chunk1 + "\n\n" + chunk2 + "\n\n" + chunk3
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	query := "relevant info"
	embedder := &fakeEmbedder{vectors: map[string][]float64{
		query:  {1, 0, 0},
		chunk1: {0.9, 0.1, 0},
		chunk2: {0, 0, 1},
		chunk3: {0.8, 0.2, 0},
	}}

	corpus := NewCorpus(s, ns, proj).
		WithChunkFilter(embedder, query).
		WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	result := string(data)

	if !strings.Contains(result, "[lines ") {
		t.Error("position markers missing from filtered output")
	}
	if !strings.Contains(result, "relevant chunk here") {
		t.Error("first relevant chunk missing")
	}
	if !strings.Contains(result, "another relevant chunk at end") {
		t.Error("last relevant chunk missing")
	}
	if strings.Contains(result, "irrelevant middle section") {
		t.Error("irrelevant chunk should be filtered out")
	}
}

func TestChunkFilter_RangedRead_SkipsFiltering(t *testing.T) {
	s := newStore(t)
	ns, proj := "ns", "cf6"
	project(t, s, ns, proj)

	content := "line0\nline1\nline2\nline3\nline4"
	write(t, s, ns, proj, "doc.md", content)
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}

	embedder := &fakeEmbedder{vectors: map[string][]float64{}}
	corpus := NewCorpus(s, ns, proj).
		WithChunkFilter(embedder, "anything").
		WithLogger(slog.Default())
	ctx := context.Background()

	data, err := corpus.Read(ctx, "doc.md", 2, 2)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "line2\nline3" {
		t.Errorf("ranged read should skip chunk filter and return exact lines, got %q", data)
	}
}
