package tools

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/authz"
)

type CopyFileInput struct {
	SourceNamespace   string `json:"source_namespace" jsonschema:"required, the source namespace"`
	SourceProjectName string `json:"source_project_name" jsonschema:"required, the source project"`
	SourcePath        string `json:"source_path" jsonschema:"required, the source file path"`
	Namespace         string `json:"namespace" jsonschema:"required, the destination namespace"`
	ProjectName       string `json:"project_name" jsonschema:"required, the destination project"`
	Path              string `json:"path,omitempty" jsonschema:"optional, the destination file path (defaults to the source filename at the destination project root); can include directory path (e.g. reports/copied.md) to place in a subdirectory"`
}

type CopyFileOutput struct {
	Message string `json:"message,omitempty"`
	ETag    string `json:"etag,omitempty"`
}

func CopyFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, CopyFileInput) (*mcp.CallToolResult, CopyFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input CopyFileInput) (*mcp.CallToolResult, CopyFileOutput, error) {
		if input.SourceNamespace == "" || input.SourceProjectName == "" || input.SourcePath == "" ||
			input.Namespace == "" || input.ProjectName == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "source_namespace, source_project_name, source_path, namespace, and project_name are required"}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		if !utils.IsValidName(input.SourceNamespace) || !utils.IsValidName(input.SourceProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid source namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid destination namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		// The authz middleware checks destination (namespace/project_name) at write level.
		// We must explicitly verify read access on the source.
		p, _ := auth.PrincipalFrom(ctx)
		if err := authz.Authorize(p.Scope, input.SourceNamespace, input.SourceProjectName, authz.LevelRead); err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "unauthorized: insufficient read access on source project"}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		// Read source file.
		content, _, err := s.ReadFileWithETag(input.SourceNamespace, input.SourceProjectName, input.SourcePath)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read source file: %v", err)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		// Determine destination path.
		destPath := input.Path
		if destPath == "" {
			destPath = path.Base(input.SourcePath)
		}

		// Check destination does not already exist.
		if _, _, rerr := s.ReadFileWithETag(input.Namespace, input.ProjectName, destPath); rerr == nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("destination file already exists: %s", destPath)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		} else if !strings.Contains(rerr.Error(), "no such file") && !strings.Contains(rerr.Error(), "not exist") {
			// If the error is something other than "not found", it's a real problem
			// (e.g. project not found, dangerous state). Surface it.
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to check destination: %v", rerr)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		// Write to destination.
		ctx = withWriteIdentity(ctx, req)
		ctx = notify.WithSender(ctx, mcpSender(req))
		etag, err := s.Write(ctx, "", input.Namespace, input.ProjectName, destPath, content, nil)
		if err != nil {
			if text, _, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: text}},
					IsError: true,
				}, CopyFileOutput{Message: reason}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to write destination file: %v", err)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		return nil, CopyFileOutput{
			Message: fmt.Sprintf("File copied from %s/%s/%s to %s/%s/%s",
				input.SourceNamespace, input.SourceProjectName, input.SourcePath,
				input.Namespace, input.ProjectName, destPath),
			ETag: etag,
		}, nil
	}
}
