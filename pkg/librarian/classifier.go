package librarian

import (
	"context"
	"sync"

	"github.com/sopranoworks/shoka/pkg/librarian/classifier"
	"github.com/sopranoworks/shoka/pkg/librarian/classifier/util"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"

	bolt "go.etcd.io/bbolt"
)

// Classifier provides vector embedding and similarity search capabilities.
type Classifier interface {
	Embed(ctx context.Context, text string) (*llm.EmbeddingVector, error)
	FindSimilar(ctx context.Context, text string, n int) ([]classifier.MatchResult, error)
	Store(ctx context.Context, key string, text string) error
}

type classifierImpl struct {
	embedder llm.Embedder
	matcher  *classifier.Matcher
	db       *bolt.DB
	model    string

	mu    sync.Mutex
	store *util.BboltStore // lazily initialized on first embedding
}

// NewClassifier builds a Classifier backed by the given embedder and bbolt DB.
// The store dimensions are determined lazily from the first embedding response.
func NewClassifier(embedder llm.Embedder, db *bolt.DB, model string) Classifier {
	return &classifierImpl{
		embedder: embedder,
		matcher:  &classifier.Matcher{},
		db:       db,
		model:    model,
	}
}

func (c *classifierImpl) ensureStore(dims int) *util.BboltStore {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = util.NewBboltStore(c.db, c.model, dims)
	}
	return c.store
}

func (c *classifierImpl) getStore() *util.BboltStore {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store
}

func (c *classifierImpl) Embed(ctx context.Context, text string) (*llm.EmbeddingVector, error) {
	vec, err := c.embedder.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	c.ensureStore(vec.Dimensions)
	return vec, nil
}

func (c *classifierImpl) FindSimilar(ctx context.Context, text string, n int) ([]classifier.MatchResult, error) {
	vec, err := c.embedder.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	store := c.ensureStore(vec.Dimensions)
	return c.matcher.FindTopN(ctx, vec, store.Iterator(), n)
}

func (c *classifierImpl) Store(ctx context.Context, key string, text string) error {
	vec, err := c.embedder.Embed(ctx, text)
	if err != nil {
		return err
	}
	store := c.ensureStore(vec.Dimensions)
	return store.Write(key, vec.Values)
}
