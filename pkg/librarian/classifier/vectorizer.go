package classifier

import (
	"context"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

type Vector = llm.EmbeddingVector

type Vectorizer struct {
	embedder llm.Embedder
}

func NewVectorizer(embedder llm.Embedder) *Vectorizer {
	return &Vectorizer{embedder: embedder}
}

func (v *Vectorizer) Embed(ctx context.Context, text string) (*Vector, error) {
	return v.embedder.Embed(ctx, text)
}
