package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/translation"
	"github.com/sopranoworks/shoka/internal/utils"
)

type TranslateFileInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the Markdown file to translate"`
	TargetLang  string `json:"target_lang,omitempty" jsonschema:"optional, the target language code (e.g., 'en', 'ja') (defaults to 'en')"`
}

type TranslateFileOutput struct {
	OutputPath string `json:"output_path"`
	Message    string `json:"message"`
}

func TranslateFileHandler(s storage.StorageService, ts translation.TranslationService) func(context.Context, *mcp.CallToolRequest, TranslateFileInput) (*mcp.CallToolResult, TranslateFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input TranslateFileInput) (*mcp.CallToolResult, TranslateFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and path are required"}},
				IsError: true,
			}, TranslateFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if input.TargetLang == "" {
			input.TargetLang = "en"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name"}},
				IsError: true,
			}, TranslateFileOutput{}, nil
		}

		// 1. Read original file
		content, err := s.ReadFile(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read file: %v", err)}},
				IsError: true,
			}, TranslateFileOutput{}, nil
		}

		// 2. Translate content
		translated, err := ts.Translate(ctx, content, input.TargetLang)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("translation failed: %v", err)}},
				IsError: true,
			}, TranslateFileOutput{}, nil
		}

		// 3. Determine output path
		ext := filepath.Ext(input.Path)
		base := strings.TrimSuffix(input.Path, ext)
		outputPath := fmt.Sprintf("%s.%s%s", base, input.TargetLang, ext)

		// 4. Write translated content
		err = s.WriteFile(input.Namespace, input.ProjectName, outputPath, translated)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to write translated file: %v", err)}},
				IsError: true,
			}, TranslateFileOutput{}, nil
		}

		return nil, TranslateFileOutput{
			OutputPath: outputPath,
			Message:    fmt.Sprintf("File translated to %s successfully", input.TargetLang),
		}, nil
	}
}
