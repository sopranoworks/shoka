package translation

import (
	"context"
)

// TranslationService defines the interface for translating text.
type TranslationService interface {
	Translate(ctx context.Context, text string, targetLang string) (string, error)
	Close() error
}
