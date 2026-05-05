package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"shoka/internal/storage"
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
			return mcp.NewToolResultError("project_name is required"), CreateProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}

		err := s.CreateProject(input.Namespace, input.ProjectName)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create project: %v", err)), CreateProjectOutput{}, nil
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

		projects, err := s.ListProjects(input.Namespace)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to list projects: %v", err)), ListProjectsOutput{}, nil
		}

		return nil, ListProjectsOutput{Projects: projects}, nil
	}
}
