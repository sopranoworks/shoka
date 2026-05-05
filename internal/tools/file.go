package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"shoka/internal/storage"
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
			return mcp.NewToolResultError("project_name and path are required"), ReadFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		content, err := s.ReadFile(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read file: %v", err)), ReadFileOutput{}, nil
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
			return mcp.NewToolResultError("project_name and path are required"), WriteFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		err := s.WriteFile(input.Namespace, input.ProjectName, input.Path, input.Content)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to write file: %v", err)), WriteFileOutput{}, nil
		}

		return nil, WriteFileOutput{Message: fmt.Sprintf("File %s written successfully", input.Path)}, nil
	}
}
