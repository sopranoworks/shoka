package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

type ReadFileInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to read"`
}

type ReadFileOutput struct {
	Content string `json:"content"`
}

func ReadFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and path are required"}},
				IsError: true,
			}, ReadFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ReadFileOutput{}, nil
		}

		content, err := s.ReadFile(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read file: %v", err)}},
				IsError: true,
			}, ReadFileOutput{}, nil
		}

		return nil, ReadFileOutput{Content: content}, nil
	}
}

type WriteFileInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to write"`
	Content     string `json:"content" jsonschema:"required, the content to write to the file"`
}

type WriteFileOutput struct {
	Message string `json:"message"`
}

func WriteFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and path are required"}},
				IsError: true,
			}, WriteFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, WriteFileOutput{}, nil
		}

		err := s.WriteFile(input.Namespace, input.ProjectName, input.Path, input.Content)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to write file: %v", err)}},
				IsError: true,
			}, WriteFileOutput{}, nil
		}

		return nil, WriteFileOutput{Message: fmt.Sprintf("File %s written successfully", input.Path)}, nil
	}
}

type DeleteFileInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to delete"`
}

type DeleteFileOutput struct {
	Message string `json:"message"`
}

func DeleteFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and path are required"}},
				IsError: true,
			}, DeleteFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, DeleteFileOutput{}, nil
		}

		err := s.DeleteFile(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to delete file: %v", err)}},
				IsError: true,
			}, DeleteFileOutput{}, nil
		}

		return nil, DeleteFileOutput{Message: fmt.Sprintf("File %s deleted successfully", input.Path)}, nil
	}
}

type GetHistoryInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"optional, the path to the file to get history for (if empty, returns project history)"`
	Limit       int    `json:"limit" jsonschema:"optional, the maximum number of history entries to return (defaults to 10)"`
}

type GetHistoryOutput struct {
	History []storage.CommitInfo `json:"history"`
}

func GetHistoryHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, GetHistoryInput) (*mcp.CallToolResult, GetHistoryOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input GetHistoryInput) (*mcp.CallToolResult, GetHistoryOutput, error) {
		if input.ProjectName == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name is required"}},
				IsError: true,
			}, GetHistoryOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if input.Limit <= 0 {
			input.Limit = 10
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, GetHistoryOutput{}, nil
		}

		history, err := s.GetHistory(input.Namespace, input.ProjectName, input.Path, input.Limit)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to get history: %v", err)}},
				IsError: true,
			}, GetHistoryOutput{}, nil
		}

		return nil, GetHistoryOutput{History: history}, nil
	}
}

type ReadFileAtVersionInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to read"`
	CommitHash  string `json:"commit_hash" jsonschema:"required, the Git commit hash to read the file from"`
}

type ReadFileAtVersionOutput struct {
	Content string `json:"content"`
}

func ReadFileAtVersionHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ReadFileAtVersionInput) (*mcp.CallToolResult, ReadFileAtVersionOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ReadFileAtVersionInput) (*mcp.CallToolResult, ReadFileAtVersionOutput, error) {
		if input.ProjectName == "" || input.Path == "" || input.CommitHash == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name, path, and commit_hash are required"}},
				IsError: true,
			}, ReadFileAtVersionOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ReadFileAtVersionOutput{}, nil
		}

		content, err := s.ReadFileAtVersion(input.Namespace, input.ProjectName, input.Path, input.CommitHash)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read file at version: %v", err)}},
				IsError: true,
			}, ReadFileAtVersionOutput{}, nil
		}

		return nil, ReadFileAtVersionOutput{Content: content}, nil
	}
}
