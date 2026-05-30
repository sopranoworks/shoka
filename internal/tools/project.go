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
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, the namespace to scope to; when omitted, projects from all namespaces are returned"`
}

type ListProjectsOutput struct {
	Projects []string `json:"projects"`
}

// ListProjectsHandler returns "<namespace>/<name>" entries. With no namespace
// argument it returns every project across all namespaces; with an explicit
// namespace it returns only that namespace's projects, in the same prefixed
// shape. (Restores the B-13 namespace surface; see B-22.)
func ListProjectsHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ListProjectsInput) (*mcp.CallToolResult, ListProjectsOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListProjectsInput) (*mcp.CallToolResult, ListProjectsOutput, error) {
		// No namespace → all namespaces, "<ns>/<name>", sorted.
		if input.Namespace == "" {
			projects, err := s.ListAllProjects()
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to list projects: %v", err)}},
					IsError: true,
				}, ListProjectsOutput{}, nil
			}
			return nil, ListProjectsOutput{Projects: projects}, nil
		}

		// Explicit namespace → only that namespace, same "<ns>/<name>" shape.
		if !utils.IsValidName(input.Namespace) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ListProjectsOutput{}, nil
		}

		names, err := s.ListProjects(input.Namespace)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to list projects: %v", err)}},
				IsError: true,
			}, ListProjectsOutput{}, nil
		}
		projects := make([]string, 0, len(names))
		for _, name := range names {
			projects = append(projects, input.Namespace+"/"+name)
		}
		return nil, ListProjectsOutput{Projects: projects}, nil
	}
}

type ListFilesInput struct {
	Namespace        string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName      string `json:"project_name" jsonschema:"required, the name of the project"`
	Path             string `json:"path,omitempty" jsonschema:"optional, the path to list files from (defaults to root)"`
	IncludeSummaries bool   `json:"include_summaries,omitempty" jsonschema:"optional, when true include each file's frontmatter, first heading, and etag in 'summaries' so an overview can be built without reading full files"`
}

// FileSummary is the per-file frontmatter, first heading, and etag returned by
// list_files when include_summaries is set.
type FileSummary struct {
	Frontmatter map[string]any `json:"frontmatter,omitempty"`
	Heading     string         `json:"heading,omitempty"`
	ETag        string         `json:"etag,omitempty"`
}

type ListFilesOutput struct {
	Files []string `json:"files"`
	// Summaries maps each (non-directory) file name to its frontmatter, heading,
	// and etag. Populated only when include_summaries is true.
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

		if input.IncludeSummaries {
			summaries := make(map[string]FileSummary)
			for _, f := range files {
				if strings.HasSuffix(f, "/") {
					continue // directory
				}
				full := filepath.Join(input.Path, f)
				content, etag, rerr := s.ReadFileWithETag(input.Namespace, input.ProjectName, full)
				if rerr != nil {
					continue
				}
				sum := markdown.Parse(content)
				summaries[f] = FileSummary{Frontmatter: sum.Frontmatter, Heading: sum.Heading, ETag: etag}
			}
			out.Summaries = summaries
		}

		return nil, out, nil
	}
}
