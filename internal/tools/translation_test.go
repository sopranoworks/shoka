package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage"
)

type mockTranslationService struct{}

func (m *mockTranslationService) Translate(ctx context.Context, text, targetLang string) (string, error) {
	return "Translated: " + text, nil
}
func (m *mockTranslationService) Close() error { return nil }

type mockStorageService struct {
	storage.StorageService
	files map[string]string
}

func (m *mockStorageService) ReadFile(ns, proj, path string) (string, error) {
	if content, ok := m.files[path]; ok {
		return content, nil
	}
	return "", fmt.Errorf("not found")
}
func (m *mockStorageService) WriteFile(ns, proj, path, content string) error {
	m.files[path] = content
	return nil
}

func TestTranslateFileHandler(t *testing.T) {
	ms := &mockStorageService{files: map[string]string{"test.md": "Hello"}}
	mts := &mockTranslationService{}
	handler := TranslateFileHandler(ms, mts)

	ctx := context.Background()
	input := TranslateFileInput{
		ProjectName: "test-proj",
		Path:        "test.md",
		TargetLang:  "en",
	}

	_, output, err := handler(ctx, nil, input)
	if err != nil {
		t.Fatalf("handler failed: %v", err)
	}

	if output.OutputPath != "test.en.md" {
		t.Errorf("expected output path test.en.md, got %s", output.OutputPath)
	}

	if ms.files["test.en.md"] != "Translated: Hello" {
		t.Errorf("expected translated content 'Translated: Hello', got %s", ms.files["test.en.md"])
	}
}
