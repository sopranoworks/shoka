// Package librariansrc is the Shoka-side data source for the ask_the_librarian
// module (backlog B-73): it adapts a Shoka project (a namespace/project on the
// storage layer) to the provider-neutral librarian.Corpus interface.
//
// This adapter — NOT pkg/librarian — is where the dependency on internal/storage
// lives, keeping pkg/librarian free of internal/storage and go-git (the archlint
// boundary). The librarian's guard (root-confinement + symlink-skip + ignore)
// still wraps every call, so this adapter only does raw, project-scoped access.
package librariansrc

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/librarian"
)

// Store is the minimal slice of the storage layer the adapter needs.
// *storage.FSGitStorage satisfies it.
type Store interface {
	SearchFiles(namespace, projectName, query, searchIn string) ([]storage.SearchMatch, error)
	ReadFile(namespace, projectName, path string) (string, error)
	ListFiles(namespace, projectName, path string) ([]string, map[string]time.Time, error)
}

// VectorSearcher performs vector similarity search for a project.
// *storage.FSGitStorage satisfies it via VectorSearch.
type VectorSearcher interface {
	VectorSearch(ctx context.Context, namespace, projectName, query string, limit int) ([]storage.VectorSearchResult, error)
}

// Corpus adapts one Shoka project to librarian.Corpus.
type Corpus struct {
	store     Store
	vec       VectorSearcher // nil when classifier not configured
	namespace string
	project   string
}

// NewCorpus binds the adapter to a single namespace/project on the given store.
func NewCorpus(store Store, namespace, project string) *Corpus {
	return &Corpus{store: store, namespace: namespace, project: project}
}

// WithVectorSearch attaches a vector searcher for hybrid search. When set,
// Search runs fulltext and vector in parallel, merging results.
func (c *Corpus) WithVectorSearch(v VectorSearcher) *Corpus {
	c.vec = v
	return c
}

var _ librarian.Corpus = (*Corpus)(nil)

// Search runs fulltext content search and, when a vector searcher is configured,
// also runs vector similarity search in parallel. Results are merged (union,
// deduplicated by path). Vector search failure is non-fatal — falls back to
// fulltext only.
func (c *Corpus) Search(ctx context.Context, query string, limit int) ([]librarian.Hit, error) {
	if limit <= 0 {
		limit = 20
	}

	if c.vec == nil {
		return c.fulltextSearch(query, limit)
	}

	// Run fulltext and vector search in parallel.
	type ftResult struct {
		hits []librarian.Hit
		err  error
	}
	type vecResult struct {
		results []storage.VectorSearchResult
		err     error
	}

	var wg sync.WaitGroup
	var ft ftResult
	var vr vecResult

	wg.Add(2)
	go func() {
		defer wg.Done()
		ft.hits, ft.err = c.fulltextSearch(query, limit)
	}()
	go func() {
		defer wg.Done()
		vr.results, vr.err = c.vec.VectorSearch(ctx, c.namespace, c.project, query, limit)
	}()
	wg.Wait()

	// Fulltext error is fatal (same as before).
	if ft.err != nil {
		return nil, ft.err
	}

	// Vector error is non-fatal — just use fulltext results.
	if vr.err != nil || len(vr.results) == 0 {
		return ft.hits, nil
	}

	// Merge: fulltext hits first (they have snippets/offsets), then add
	// vector-only hits that weren't in fulltext.
	seen := make(map[string]bool, len(ft.hits))
	merged := make([]librarian.Hit, 0, len(ft.hits)+len(vr.results))
	for _, h := range ft.hits {
		seen[h.Path] = true
		merged = append(merged, h)
	}
	for _, r := range vr.results {
		if seen[r.Path] {
			continue
		}
		seen[r.Path] = true
		merged = append(merged, librarian.Hit{
			Path:    r.Path,
			Snippet: "(semantic match)",
			Offset:  0,
		})
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

func (c *Corpus) fulltextSearch(query string, limit int) ([]librarian.Hit, error) {
	matches, err := c.store.SearchFiles(c.namespace, c.project, query, "content")
	if err != nil {
		return nil, err
	}
	hits := make([]librarian.Hit, 0, len(matches))
	for _, m := range matches {
		if limit > 0 && len(hits) >= limit {
			break
		}
		hits = append(hits, librarian.Hit{Path: m.Path, Snippet: m.Snippet, Offset: m.Offset})
	}
	return hits, nil
}

// Read returns only the [offset, offset+limit) line span of the file. The whole
// file is loaded by storage (as any read does), but ONLY the bounded span is
// returned, so a huge file never enters the librarian's context — the B-73
// guarantee (design report §1.3). limit <= 0 means to end.
func (c *Corpus) Read(_ context.Context, path string, offset, limit int) ([]byte, error) {
	content, err := c.store.ReadFile(c.namespace, c.project, path)
	if err != nil {
		return nil, err
	}
	return []byte(librarian.SliceLines(content, offset, limit)), nil
}

// List maps Shoka's catalog listing (leaf names; directories carry a trailing
// "/") to librarian entries.
func (c *Corpus) List(_ context.Context, dir string) ([]librarian.Entry, error) {
	names, _, err := c.store.ListFiles(c.namespace, c.project, dir)
	if err != nil {
		return nil, err
	}
	entries := make([]librarian.Entry, 0, len(names))
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			entries = append(entries, librarian.Entry{Name: strings.TrimSuffix(n, "/"), IsDir: true})
		} else {
			entries = append(entries, librarian.Entry{Name: n, IsDir: false})
		}
	}
	return entries, nil
}
