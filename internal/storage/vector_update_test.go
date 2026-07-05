package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/vectorindex"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEmbedder is a test embedder that returns a deterministic vector.
type fakeEmbedder struct {
	mu         sync.Mutex
	calls      int
	failNext   bool
	dimensions int
}

func newFakeEmbedder(dims int) *fakeEmbedder {
	return &fakeEmbedder{dimensions: dims}
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) (*llm.EmbeddingVector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failNext {
		f.failNext = false
		return nil, context.DeadlineExceeded
	}
	vec := make([]float64, f.dimensions)
	for i := range vec {
		if i < len(text) {
			vec[i] = float64(text[i]) / 255.0
		}
	}
	return &llm.EmbeddingVector{
		Model:      "test-model",
		Dimensions: f.dimensions,
		Values:     vec,
	}, nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeEmbedder) setFailNext() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = true
}

func setupVectorStore(t *testing.T) (*FSGitStorage, *fakeEmbedder) {
	t.Helper()
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	embedder := newFakeEmbedder(4)
	s.SetVectorConfig(&VectorIndexConfig{
		Embedder:   embedder,
		Model:      "test-model",
		Dimensions: 4,
	})
	return s, embedder
}

// waitForVectorEmbedded waits until at least n successful embeddings are stored.
func waitForVectorEmbedded(t *testing.T, s *FSGitStorage, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		_, _, embedded, _, _, _ := s.VectorCounters()
		if int(embedded) >= n {
			return
		}
		select {
		case <-deadline:
			_, _, emb, fail, _, _ := s.VectorCounters()
			t.Fatalf("timeout waiting for vector embeds: embedded=%d failed=%d, want embedded>=%d",
				emb, fail, n)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// waitForVectorProcessed waits until at least n items have been processed
// (embedded + failed).
func waitForVectorProcessed(t *testing.T, s *FSGitStorage, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		_, _, embedded, failed, _, _ := s.VectorCounters()
		if int(embedded+failed) >= n {
			return
		}
		select {
		case <-deadline:
			_, _, emb, fail, _, _ := s.VectorCounters()
			t.Fatalf("timeout waiting for vector processing: embedded=%d failed=%d, want total>=%d",
				emb, fail, n)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestVector_WriteEnqueuesAndWorkerProcesses(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0) // no periodic sweep

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello world", nil)
	require.NoError(t, err)

	waitForVectorEmbedded(t, s, 1)

	// Verify vector was stored
	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	vec, found, err := st.Get("a.md")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Len(t, vec, 4)
}

func TestVector_DeleteRemovesEntry(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "content", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	// Delete the file
	require.NoError(t, s.Delete(context.Background(), "sess", "ns", "proj", "a.md", nil))

	// vectorDelete is synchronous — verify immediately
	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	_, found, err := st.Get("a.md")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestVector_MoveDeletesSourceEnqueuesDest(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "src.md", "body", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "src.md", "dst.md", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 2)

	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)

	// Source should be gone
	_, found, _ := st.Get("src.md")
	assert.False(t, found)

	// Dest should exist
	_, found, _ = st.Get("dst.md")
	assert.True(t, found)
}

func TestVector_NotConfigured_WritePathUnchanged(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	// No SetVectorConfig — vector indexing disabled

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	// No vector store should exist
	st := s.vectorForRead("ns", "proj")
	assert.Nil(t, st)
}

func TestVector_EmbedFailure_DoesNotCrash(t *testing.T) {
	s, embedder := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	embedder.setFailNext()
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	// Wait for the worker to process (it will fail)
	waitForVectorProcessed(t, s, 1)

	// The write succeeded even though embedding failed
	content, err := s.ReadFile("ns", "proj", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "hello", content)

	// Vector entry should not exist (embed failed)
	st := s.vectorForRead("ns", "proj")
	if st != nil {
		_, found, _ := st.Get("a.md")
		assert.False(t, found)
	}
}

func TestVector_ModelChange_DiscardsStore(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	// Verify it's stored with the correct model
	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	require.NoError(t, st.CheckModel("test-model", 4))

	// Simulate model change: replace the store with one tagged "old-model"
	p := s.vectorPath("ns", "proj")
	s.removeVectorFile("ns", "proj")
	st2, err := vectorindex.Create(p, "ns", "proj", "old-model", 4)
	require.NoError(t, err)
	require.NoError(t, st2.Put("a.md", []float64{1, 2, 3, 4}))
	s.registerVectorStore("ns", "proj", st2)

	// Write again — the worker detects model mismatch and discards the store
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "world", nil)
	require.NoError(t, err)
	// The embed will succeed (API call works) but CheckModel fails, so the store
	// is removed. Wait for the item to be processed (counted as a failed embed).
	waitForVectorProcessed(t, s, 2)

	// The old store with "old-model" should be removed from disk
	_, openErr := vectorindex.Open(p)
	assert.ErrorIs(t, openErr, vectorindex.ErrNotFound,
		"vector DB should have been removed on model mismatch")
}

func TestVector_SiblingRegistered(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	paths := s.siblingDBPaths("ns", "proj")
	found := false
	for _, p := range paths {
		if p == s.vectorPath("ns", "proj") {
			found = true
			break
		}
	}
	assert.True(t, found, "vector.db must be in siblingDBPaths")
}
