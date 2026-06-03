package translation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGoogleTranslationService_ImplementsInterface(t *testing.T) {
	var _ TranslationService = (*GoogleTranslationService)(nil)
}

func TestNewGoogleTranslationService(t *testing.T) {
	ctx := context.Background()
	// We don't provide credentials, so it might fail or succeed depending on environment.
	// The goal here is to ensure the function is callable and returns the expected type.
	service, err := NewGoogleTranslationService(ctx, "test-project")

	if err != nil {
		t.Logf("NewGoogleTranslationService failed (expected if no credentials): %v", err)
		return
	}

	assert.NotNil(t, service)
	assert.Equal(t, "test-project", service.projectID)
	err = service.Close()
	assert.NoError(t, err)
}
