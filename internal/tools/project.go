package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

type CreateProjectInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project to create"`
}

type CreateProjectOutput struct {
	Message string `json:"message"`
}

func CreateProjectHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, CreateProjectInput) (*mcp.CallToolResult, CreateProjectOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input CreateProjectInput) (*mcp.CallToolResult, CreateProjectOutput, error) {
		if input.ProjectName == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name is required"}},
				IsError: true,
			}, CreateProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, CreateProjectOutput{}, nil
		}

		err := s.CreateProject(input.Namespace, input.ProjectName)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to create project: %v", err)}},
				IsError: true,
			}, CreateProjectOutput{}, nil
		}

		return nil, CreateProjectOutput{Message: fmt.Sprintf("Project %s/%s created successfully", input.Namespace, input.ProjectName)}, nil
	}
}

type ListProjectsInput struct {
	Namespace string `json:"namespace" jsonschema:"optional, the namespace to list projects from (defaults to 'default')"`
}

type ListProjectsOutput struct {
	Projects []string `json:"projects"`
}

func ListProjectsHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ListProjectsInput) (*mcp.CallToolResult, ListProjectsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListProjectsInput) (*mcp.CallToolResult, ListProjectsOutput, error) {
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ListProjectsOutput{}, nil
		}

		projects, err := s.ListProjects(input.Namespace)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to list projects: %v", err)}},
				IsError: true,
			}, ListProjectsOutput{}, nil
		}

		return nil, ListProjectsOutput{Projects: projects}, nil
	}
}

type ListFilesInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"optional, the path to list files from (defaults to root)"`
}

type ListFilesOutput struct {
	Files []string `json:"files"`
}

func ListFilesHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ListFilesInput) (*mcp.CallToolResult, ListFilesOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListFilesInput) (*mcp.CallToolResult, ListFilesOutput, error) {
		if input.ProjectName == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name is required"}},
				IsError: true,
			}, ListFilesOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ListFilesOutput{}, nil
		}

		files, err := s.ListFiles(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to list files: %v", err)}},
				IsError: true,
			}, ListFilesOutput{}, nil
		}

		return nil, ListFilesOutput{Files: files}, nil
	}
}
