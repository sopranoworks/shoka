package translation

import (
	"context"
	"fmt"

	"cloud.google.com/go/translate/apiv3"
	"google.golang.org/api/option"
	translatepb "google.golang.org/genproto/googleapis/cloud/translate/v3"
)

// GoogleTranslationService implements TranslationService using Google Cloud Translation API.
type GoogleTranslationService struct {
	client    *translate.TranslationClient
	projectID string
}

// NewGoogleTranslationService creates a new GoogleTranslationService.
func NewGoogleTranslationService(ctx context.Context, projectID string, opts ...option.ClientOption) (*GoogleTranslationService, error) {
	client, err := translate.NewTranslationClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create translation client: %w", err)
	}
	return &GoogleTranslationService{
		client:    client,
		projectID: projectID,
	}, nil
}

// Translate translates the given text to the target language.
func (s *GoogleTranslationService) Translate(ctx context.Context, text string, targetLang string) (string, error) {
	req := &translatepb.TranslateTextRequest{
		Parent:             fmt.Sprintf("projects/%s/locations/global", s.projectID),
		TargetLanguageCode: targetLang,
		Contents:           []string{text},
		MimeType:           "text/plain", // Markdown is plain text for translation purposes
	}

	resp, err := s.client.TranslateText(ctx, req)
	if err != nil {
		return "", fmt.Errorf("translation request failed: %w", err)
	}

	translations := resp.GetTranslations()
	if len(translations) == 0 {
		return "", fmt.Errorf("no translations returned")
	}

	return translations[0].TranslatedText, nil
}

// Close closes the underlying translation client.
func (s *GoogleTranslationService) Close() error {
	return s.client.Close()
}
