package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/markdown"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

type CreateProjectInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
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
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, the namespace to list projects from (defaults to 'default')"`
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
	Namespace        string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName      string `json:"project_name" jsonschema:"required, the name of the project"`
	Path             string `json:"path,omitempty" jsonschema:"optional, the path to list files from (defaults to root)"`
	IncludeVersions  bool   `json:"include_versions,omitempty" jsonschema:"optional, when true include each file's current commit hash in 'versions'"`
	IncludeSummaries bool   `json:"include_summaries,omitempty" jsonschema:"optional, when true include each file's frontmatter and first heading in 'summaries' so an overview can be built without reading full files"`
}

// FileSummary is the per-file frontmatter + first heading returned by
// list_files when include_summaries is set.
type FileSummary struct {
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Heading     string         `json:"heading,omitempty"`
}

type ListFilesOutput struct {
	Files []string `json:"files"`
	// Versions maps each (non-directory) file name to its current commit hash.
	// Populated only when include_versions is true.
	Versions map[string]string `json:"versions,omitempty"`
	// Summaries maps each (non-directory) file name to its frontmatter + heading.
	// Populated only when include_summaries is true.
	Summaries map[string]FileSummary `json:"summaries,omitempty"`
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

		out := ListFilesOutput{Files: files}

		if input.IncludeVersions {
			versions := make(map[string]string)
			for _, f := range files {
				if strings.HasSuffix(f, "/") {
					continue // directory
				}
				full := filepath.Join(input.Path, f)
				if v, verr := s.GetCurrentVersion(input.Namespace, input.ProjectName, full); verr == nil {
					versions[f] = v
				}
			}
			out.Versions = versions
		}

		if input.IncludeSummaries {
			summaries := make(map[string]FileSummary)
			for _, f := range files {
				if strings.HasSuffix(f, "/") {
					continue // directory
				}
				full := filepath.Join(input.Path, f)
				content, rerr := s.ReadFile(input.Namespace, input.ProjectName, full)
				if rerr != nil {
					continue
				}
				sum := markdown.Parse(content)
				summaries[f] = FileSummary{Frontmatter: sum.Frontmatter, Heading: sum.Heading}
			}
			out.Summaries = summaries
		}

		return nil, out, nil
	}
}
