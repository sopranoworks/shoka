package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/markdown"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

type ReadSummaryInput struct {
	Namespace   string `json:"namespace" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the Markdown file to summarize"`
}

// ReadSummaryOutput is a context-efficient view of a file. It never contains the
// full body — only a capped excerpt.
type ReadSummaryOutput struct {
	Frontmatter map[string]any `json:"frontmatter"`
	Heading     string         `json:"heading"`
	Excerpt     string         `json:"excerpt"`
	Size        int            `json:"size"`
	Version     string         `json:"version,omitempty"`
	ModifiedAt  string         `json:"modified_at,omitempty"`
}

func ReadSummaryHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ReadSummaryInput) (*mcp.CallToolResult, ReadSummaryOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ReadSummaryInput) (*mcp.CallToolResult, ReadSummaryOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name and path are required"}},
				IsError: true,
			}, ReadSummaryOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, ReadSummaryOutput{}, nil
		}

		content, err := s.ReadFile(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read file: %v", err)}},
				IsError: true,
			}, ReadSummaryOutput{}, nil
		}

		sum := markdown.Parse(content)
		out := ReadSummaryOutput{
			Frontmatter: sum.Frontmatter,
			Heading:     sum.Heading,
			Excerpt:     sum.Excerpt,
			Size:        len(content),
		}

		if hist, herr := s.GetHistory(input.Namespace, input.ProjectName, input.Path, 1); herr == nil && len(hist) > 0 {
			out.Version = hist[0].Hash
			out.ModifiedAt = hist[0].Date.UTC().Format(time.RFC3339)
		}

		return nil, out, nil
	}
}
