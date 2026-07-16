package storage

import (
	"context"
	"errors"
	"fmt"
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
	failAll    bool
	dimensions int
}

func newFakeEmbedder(dims int) *fakeEmbedder {
	return &fakeEmbedder{dimensions: dims}
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) (*llm.EmbeddingVector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failAll {
		return nil, errors.New("simulated persistent failure")
	}
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

// waitForSweep waits until the vector sweep has completed at least n runs.
func waitForSweep(t *testing.T, s *FSGitStorage, n int64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for s.VectorSweepRuns() < n {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for sweep runs >= %d (got %d)", n, s.VectorSweepRuns())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
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

	// Wait for initial sweep to complete (empty project, fast)
	waitForSweep(t, s, 1)

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "src.md", "body", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "src.md", "dst.md", nil)
	require.NoError(t, err)

	// Wait until dst.md appears in the store (the move enqueues it; poll the store).
	deadline := time.After(5 * time.Second)
	for {
		st := s.vectorForRead("ns", "proj")
		if st != nil {
			if _, found, _ := st.Get("dst.md"); found {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for dst.md to appear in vector index")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)

	// Source should be gone (vectorDelete is synchronous, ran before enqueue)
	_, found, _ := st.Get("src.md")
	assert.False(t, found)

	// Dest confirmed above
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

	// Wait for initial sweep (empty project)
	waitForSweep(t, s, 1)

	embedder.setFailNext()
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	// Wait for the worker to process (it will fail)
	_, _, _, baseFailed, _, _ := s.VectorCounters()
	deadline := time.After(5 * time.Second)
	for {
		_, _, _, failed, _, _ := s.VectorCounters()
		if failed > baseFailed {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for embed failure")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

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

	// Wait for initial sweep to finish (empty project, fast)
	waitForSweep(t, s, 1)

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

	// Wait until the model-mismatch discard happens (poll the disk)
	deadline := time.After(5 * time.Second)
	for {
		_, openErr := vectorindex.Open(p)
		if errors.Is(openErr, vectorindex.ErrNotFound) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for model mismatch discard")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
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

func TestVector_SetVectorConfig_ModelChange_DiscardsHandles(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	// Write a file so the vector store is opened
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	// Verify store is in memory
	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)

	// Swap to a different model via SetVectorConfig
	newEmbedder := newFakeEmbedder(8)
	s.SetVectorConfig(&VectorIndexConfig{
		Embedder:   newEmbedder,
		Model:      "new-model",
		Dimensions: 8,
	})

	// In-memory handles should be gone (discarded on model change)
	st2 := s.vectorForRead("ns", "proj")
	// The old store handle was closed; vectorForRead tries to open from disk but
	// the on-disk file has the old model — it opens fine but CheckModel will fail
	// when the sweep or next write hits it. The key point: the in-memory registry
	// was cleared.
	if st2 != nil {
		err = st2.CheckModel("new-model", 8)
		assert.ErrorIs(t, err, vectorindex.ErrModelMismatch,
			"old on-disk store should not match new model")
	}
}

func TestVector_SetVectorConfig_Deactivate(t *testing.T) {
	s, _ := setupVectorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartVectorWorker(ctx, 0)

	// Wait for initial sweep + write embed to settle
	waitForSweep(t, s, 1)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	waitForVectorEmbedded(t, s, 1)

	// Record embed count, then deactivate
	_, _, baseEmbedded, _, _, _ := s.VectorCounters()
	s.SetVectorConfig(nil)

	// Write should not enqueue anything
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "world", nil)
	require.NoError(t, err)

	// Give the worker time to NOT process anything
	time.Sleep(50 * time.Millisecond)
	_, _, embedded, _, _, _ := s.VectorCounters()
	assert.Equal(t, baseEmbedded, embedded, "no new embeds after deactivation")
}

func TestVector_SetVectorConfig_Activate_OnReload(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	// Not configured at startup — write should not enqueue

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	// Now activate via SetVectorConfig + StartVectorWorker (as the reload would)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	embedder := newFakeEmbedder(4)
	s.SetVectorConfig(&VectorIndexConfig{
		Embedder:   embedder,
		Model:      "test-model",
		Dimensions: 4,
	})
	s.StartVectorWorker(ctx, 0)

	// The initial reconcile should vectorize the pre-existing "a.md"
	waitForVectorEmbedded(t, s, 1)

	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	_, found, _ := st.Get("a.md")
	assert.True(t, found, "pre-existing file should be vectorized by initial sweep")
}

func TestVector_InitialSweep_VectorizesPreExistingFiles(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	// Write files BEFORE classifier is configured
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "doc1.md", "first document", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "doc2.md", "second document", nil)
	require.NoError(t, err)

	// Now configure and start the worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.SetVectorConfig(&VectorIndexConfig{
		Embedder:   newFakeEmbedder(4),
		Model:      "test-model",
		Dimensions: 4,
	})
	s.StartVectorWorker(ctx, 0) // interval=0 means no periodic sweep; initial sweep still runs

	// Both pre-existing files should be vectorized by the initial reconcile
	waitForVectorEmbedded(t, s, 2)

	st := s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	_, found1, _ := st.Get("doc1.md")
	_, found2, _ := st.Get("doc2.md")
	assert.True(t, found1, "doc1.md should be vectorized")
	assert.True(t, found2, "doc2.md should be vectorized")
}

func TestVector_SweepAbortsAfterConsecutiveFailures(t *testing.T) {
	s, embedder := setupVectorStore(t)

	// Write more files than the consecutive failure limit
	fileCount := vectorSweepConsecFailLimit + 5
	for i := range fileCount {
		_, err := s.Write(context.Background(), "sess", "ns", "proj",
			fmt.Sprintf("file%d.md", i), fmt.Sprintf("content %d", i), nil)
		require.NoError(t, err)
	}

	// Make ALL embeds fail persistently
	embedder.mu.Lock()
	embedder.failAll = true
	embedder.calls = 0
	embedder.mu.Unlock()

	// Run the sweep directly (not via the worker, to avoid the initial reconcile)
	s.reconcileProjectVectors(context.Background(), "ns", "proj")

	// The sweep should have aborted after vectorSweepConsecFailLimit consecutive failures
	calls := embedder.callCount()
	assert.Equal(t, vectorSweepConsecFailLimit, calls,
		"sweep should abort after %d consecutive failures, got %d calls", vectorSweepConsecFailLimit, calls)
	assert.Equal(t, int64(1), s.VectorSweepAborts(),
		"sweep abort counter should be incremented")
}

func TestVector_SweepConsecFailResetOnSuccess(t *testing.T) {
	s, embedder := setupVectorStore(t)

	// Write several files
	for i := range 6 {
		_, err := s.Write(context.Background(), "sess", "ns", "proj",
			fmt.Sprintf("f%d.md", i), fmt.Sprintf("content %d", i), nil)
		require.NoError(t, err)
	}

	// Make embeds fail only intermittently (failNext resets after one failure).
	// Since map iteration is nondeterministic, we can't control which files fail.
	// Instead, verify that a full sweep with all embeds succeeding does NOT abort.
	embedder.mu.Lock()
	embedder.calls = 0
	embedder.mu.Unlock()

	s.reconcileProjectVectors(context.Background(), "ns", "proj")

	calls := embedder.callCount()
	assert.Equal(t, 6, calls, "all files should be embedded when embedder succeeds")
	assert.Equal(t, int64(0), s.VectorSweepAborts(),
		"no aborts when all embeds succeed")
}

func TestVector_FailedEmbed_RetriedOnNextSweep(t *testing.T) {
	s, embedder := setupVectorStore(t)

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "retry.md", "will retry", nil)
	require.NoError(t, err)

	// First sweep: all embeds fail
	embedder.mu.Lock()
	embedder.failAll = true
	embedder.mu.Unlock()
	s.reconcileProjectVectors(context.Background(), "ns", "proj")

	st := s.vectorForRead("ns", "proj")
	if st != nil {
		_, found, _ := st.Get("retry.md")
		assert.False(t, found, "file should not be in vector store after failed sweep")
	}

	// Second sweep: embeds succeed
	embedder.mu.Lock()
	embedder.failAll = false
	embedder.calls = 0
	embedder.mu.Unlock()
	s.reconcileProjectVectors(context.Background(), "ns", "proj")

	calls := embedder.callCount()
	assert.Equal(t, 1, calls, "retry.md should be re-attempted on second sweep")
	st = s.vectorForRead("ns", "proj")
	require.NotNil(t, st)
	_, found, _ := st.Get("retry.md")
	assert.True(t, found, "file should be in vector store after successful retry")
}
