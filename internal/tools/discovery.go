package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
)

type ListFilesSinceInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Since       string `json:"since" jsonschema:"required, an RFC3339 timestamp or a commit hash (exclusive); only files changed after this point are returned"`
}

type ListFilesSinceOutput struct {
	Changes []storage.FileChange `json:"changes"`
}

func ListFilesSinceHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ListFilesSinceInput) (*mcp.CallToolResult, ListFilesSinceOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListFilesSinceInput) (*mcp.CallToolResult, ListFilesSinceOutput, error) {
		if input.ProjectName == "" || input.Since == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and since are required"}},
				IsError: true,
			}, ListFilesSinceOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ListFilesSinceOutput{}, nil
		}

		changes, err := s.ListFilesSince(input.Namespace, input.ProjectName, input.Since)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to list changes: %v", err)}},
				IsError: true,
			}, ListFilesSinceOutput{}, nil
		}

		return nil, ListFilesSinceOutput{Changes: changes}, nil
	}
}

type SearchFilesInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Query       string `json:"query" jsonschema:"required, the case-insensitive substring to search for"`
	SearchIn    string `json:"search_in,omitempty" jsonschema:"optional, one of 'filename', 'content', or 'both' (default 'both')"`
}

type SearchFilesOutput struct {
	Matches []storage.SearchMatch `json:"matches"`
}

func SearchFilesHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, SearchFilesInput) (*mcp.CallToolResult, SearchFilesOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchFilesInput) (*mcp.CallToolResult, SearchFilesOutput, error) {
		if input.ProjectName == "" || input.Query == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and query are required"}},
				IsError: true,
			}, SearchFilesOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, SearchFilesOutput{}, nil
		}

		matches, err := s.SearchFiles(input.Namespace, input.ProjectName, input.Query, input.SearchIn)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("search failed: %v", err)}},
				IsError: true,
			}, SearchFilesOutput{}, nil
		}

		return nil, SearchFilesOutput{Matches: matches}, nil
	}
}
