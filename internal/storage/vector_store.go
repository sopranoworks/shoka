package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/sopranoworks/shoka/internal/storage/vectorindex"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// VectorIndexConfig holds the embedder configuration needed to operate the
// per-project vector index. When nil/zero the vector index is disabled and the
// write path is unaffected.
type VectorIndexConfig struct {
	Embedder   llm.Embedder
	Model      string
	Dimensions int // 0 = determine lazily from first embed response
}

// vectorPath returns a project's vector index DB path:
// <base_dir>/<namespace>/<project>.vector.db
func (s *FSGitStorage) vectorPath(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, projectName+".vector.db")
}

// SetVectorConfig injects or swaps the embedder configuration. This is called
// at startup and on config live-reload. When cfg is nil (classifier not
// configured or deactivated), the vector index is disabled but existing stores
// are left on disk (they'll be valid if the same model is re-enabled later).
// When the model changes, all in-memory stores are discarded so the next write
// or sweep detects the mismatch and rebuilds.
func (s *FSGitStorage) SetVectorConfig(cfg *VectorIndexConfig) {
	s.vecMu.Lock()
	oldCfg := s.vecConfig
	s.vecConfig = cfg
	s.vecMu.Unlock()

	// Detect model change: if the old and new configs have different models,
	// discard all in-memory vector stores. The on-disk .vector.db files are left;
	// the sweep or next write will detect the model mismatch via CheckModel and
	// rebuild from scratch.
	if oldCfg != nil && cfg != nil && oldCfg.Model != cfg.Model {
		s.discardAllVectorStores()
		s.log().Info("vector index: model changed, discarded in-memory stores",
			"old_model", oldCfg.Model, "new_model", cfg.Model)
	}
}

// discardAllVectorStores closes and removes all in-memory vector store handles.
// The on-disk files remain — they are rebuilt by the sweep when it detects the
// model mismatch.
func (s *FSGitStorage) discardAllVectorStores() {
	s.vecStoreMu.Lock()
	defer s.vecStoreMu.Unlock()
	for key, st := range s.vecStores {
		if st != nil {
			_ = st.Close()
		}
		delete(s.vecStores, key)
	}
}

// VectorConfigured reports whether the vector index is active.
func (s *FSGitStorage) VectorConfigured() bool {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	return s.vecConfig != nil
}

// vectorForRead returns the open vector store for a project without ever creating
// one. Returns nil if no store is registered and none can be opened, or if the
// model/dimensions don't match the current config (triggering a rebuild).
func (s *FSGitStorage) vectorForRead(namespace, projectName string) *vectorindex.Store {
	key := projectKey(namespace, projectName)
	s.vecStoreMu.Lock()
	defer s.vecStoreMu.Unlock()
	if st, ok := s.vecStores[key]; ok && st != nil {
		return st
	}
	st, err := vectorindex.Open(s.vectorPath(namespace, projectName))
	if err != nil {
		return nil
	}
	s.vecStores[key] = st
	return st
}

// vectorFor returns the open vector store for a project, opening or creating it
// on demand. Returns an error if the store cannot be opened/created, or if the
// model/dimensions have changed (the caller should rebuild).
func (s *FSGitStorage) vectorFor(namespace, projectName string) (*vectorindex.Store, error) {
	key := projectKey(namespace, projectName)
	s.vecStoreMu.Lock()
	defer s.vecStoreMu.Unlock()
	if st, ok := s.vecStores[key]; ok && st != nil {
		return st, nil
	}
	p := s.vectorPath(namespace, projectName)
	st, err := vectorindex.Open(p)
	if errors.Is(err, vectorindex.ErrNotFound) {
		cfg := s.currentVecConfig()
		if cfg == nil {
			return nil, fmt.Errorf("vectorindex: not configured")
		}
		dims := cfg.Dimensions
		if dims == 0 {
			dims = s.vecResolvedDims
		}
		if dims == 0 {
			return nil, fmt.Errorf("vectorindex: dimensions not yet resolved")
		}
		st, err = vectorindex.Create(p, namespace, projectName, cfg.Model, dims)
	}
	if err != nil {
		return nil, err
	}
	s.vecStores[key] = st
	return st, nil
}

// registerVectorStore records an already-open vector store handle.
func (s *FSGitStorage) registerVectorStore(namespace, projectName string, st *vectorindex.Store) {
	key := projectKey(namespace, projectName)
	s.vecStoreMu.Lock()
	if old, ok := s.vecStores[key]; ok && old != nil && old != st {
		_ = old.Close()
	}
	s.vecStores[key] = st
	s.vecStoreMu.Unlock()
}

// removeVectorFile deletes a project's on-disk vector DB and drops any registered
// handle. Used by the sweep to discard a stale/corrupt store before recreating.
func (s *FSGitStorage) removeVectorFile(namespace, projectName string) {
	key := projectKey(namespace, projectName)
	s.vecStoreMu.Lock()
	if old, ok := s.vecStores[key]; ok && old != nil {
		_ = old.Close()
		delete(s.vecStores, key)
	}
	s.vecStoreMu.Unlock()
	if err := os.Remove(s.vectorPath(namespace, projectName)); err != nil && !os.IsNotExist(err) {
		s.log().Warn("vector index file remove failed",
			"namespace", namespace, "project", projectName, "err", err)
	}
}

// vectorDelete removes a path from the vector index. Synchronous and best-effort
// (no API call needed for a delete — just a bbolt key removal).
func (s *FSGitStorage) vectorDelete(namespace, projectName, rel string) {
	if !s.VectorConfigured() {
		return
	}
	st := s.vectorForRead(namespace, projectName)
	if st == nil {
		return
	}
	if err := st.Delete(rel); err != nil {
		s.log().Warn("vector index delete failed",
			"namespace", namespace, "project", projectName, "path", rel, "err", err)
		s.vecUpdateFailedDelete.Add(1)
	}
}

// vectorEnqueue queues a file for background vectorization. Non-blocking: if the
// channel is full the item is dropped (the sweep catches it later).
func (s *FSGitStorage) vectorEnqueue(namespace, projectName, rel string, content []byte) {
	if !s.VectorConfigured() {
		return
	}
	item := vectorWorkItem{
		namespace: namespace,
		project:   projectName,
		path:      rel,
		content:   content,
	}
	select {
	case s.vecQueue <- item:
		s.vecEnqueued.Add(1)
	default:
		s.vecDropped.Add(1)
	}
}

// currentVecConfig returns the current vector config under the lock.
func (s *FSGitStorage) currentVecConfig() *VectorIndexConfig {
	s.vecMu.Lock()
	defer s.vecMu.Unlock()
	return s.vecConfig
}

// VectorCounters returns the vector index observability counters.
func (s *FSGitStorage) VectorCounters() (enqueued, dropped, embedded, failedEmbed, failedDelete, rebuilds int64) {
	return s.vecEnqueued.Load(),
		s.vecDropped.Load(),
		s.vecEmbedded.Load(),
		s.vecFailedEmbed.Load(),
		s.vecUpdateFailedDelete.Load(),
		s.vecRebuilds.Load()
}

// VectorSweepRuns returns the number of vector reconcile passes.
func (s *FSGitStorage) VectorSweepRuns() int64 { return s.vecSweepRuns.Load() }

// VectorProjectCount returns the number of projects that currently have an open
// or openable vector index (used by the UI status display).
func (s *FSGitStorage) VectorProjectCount() int {
	s.vecStoreMu.Lock()
	n := len(s.vecStores)
	s.vecStoreMu.Unlock()
	return n
}

// vectorEmbed calls the configured embedder and stores the result. Called by
// the background worker. Returns the dimensions (for lazy resolution).
func (s *FSGitStorage) vectorEmbed(ctx context.Context, namespace, projectName, rel string, content []byte) (int, error) {
	cfg := s.currentVecConfig()
	if cfg == nil {
		return 0, fmt.Errorf("vectorindex: not configured")
	}
	vec, err := cfg.Embedder.Embed(ctx, string(content))
	if err != nil {
		s.vecFailedEmbed.Add(1)
		return 0, err
	}

	// Lazy dimension resolution: the first successful embed determines dimensions.
	s.vecDimOnce.Do(func() {
		s.vecResolvedDims = vec.Dimensions
	})

	st, stErr := s.vectorFor(namespace, projectName)
	if stErr != nil {
		s.vecFailedEmbed.Add(1)
		return vec.Dimensions, stErr
	}

	// Check model/dimensions match
	if err := st.CheckModel(cfg.Model, vec.Dimensions); err != nil {
		s.vecFailedEmbed.Add(1)
		return vec.Dimensions, err
	}

	if err := st.Put(rel, vec.Values); err != nil {
		s.vecFailedEmbed.Add(1)
		return vec.Dimensions, err
	}
	s.vecEmbedded.Add(1)
	return vec.Dimensions, nil
}

// VectorSearchResult is one result from a vector similarity search.
type VectorSearchResult struct {
	Path       string
	Similarity float64
}

// VectorSearch embeds the query and searches the per-project vector index for
// the top N most similar files by cosine similarity. Returns nil (not error)
// when the vector index is not configured or the project has no vector store.
func (s *FSGitStorage) VectorSearch(ctx context.Context, namespace, projectName, query string, limit int) ([]VectorSearchResult, error) {
	cfg := s.currentVecConfig()
	if cfg == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}

	st := s.vectorForRead(namespace, projectName)
	if st == nil {
		return nil, nil
	}

	queryVec, err := cfg.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("vector search embed query: %w", err)
	}

	keys, kerr := st.Keys()
	if kerr != nil {
		return nil, fmt.Errorf("vector search keys: %w", kerr)
	}

	type scored struct {
		path string
		sim  float64
	}
	var results []scored
	for _, k := range keys {
		vec, found, gerr := st.Get(k)
		if gerr != nil || !found {
			continue
		}
		sim := cosineSimilarity(queryVec.Values, vec)
		results = append(results, scored{path: k, sim: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].sim > results[j].sim
	})
	if len(results) > limit {
		results = results[:limit]
	}

	out := make([]VectorSearchResult, len(results))
	for i, r := range results {
		out[i] = VectorSearchResult{Path: r.path, Similarity: r.sim}
	}
	return out, nil
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
