package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

type ReadFileInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to read"`
}

type ReadFileOutput struct {
	Content string `json:"content"`
	// Version is the commit hash the file is currently at, suitable for passing
	// back as expected_version on a subsequent write_file/delete_file.
	Version string `json:"version"`
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

		version, verr := s.GetCurrentVersion(input.Namespace, input.ProjectName, input.Path)
		if verr != nil {
			version = ""
		}

		return nil, ReadFileOutput{Content: content, Version: version}, nil
	}
}

type WriteFileInput struct {
	Namespace       string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName     string `json:"project_name" jsonschema:"required, the name of the project"`
	Path            string `json:"path" jsonschema:"required, the path to the file to write"`
	Content         string `json:"content" jsonschema:"required, the content to write to the file"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"optional, the commit hash the file is expected to be at (from read_file); if set and stale, the write is rejected with a version conflict and no change is made"`
}

type WriteFileOutput struct {
	Message string `json:"message"`
	// Version is the commit hash produced by this write.
	Version string `json:"version,omitempty"`
	// Conflict is true when expected_version did not match; CurrentVersion then
	// holds the file's actual current version.
	Conflict       bool   `json:"conflict,omitempty"`
	CurrentVersion string `json:"current_version,omitempty"`
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

		version, err := s.WriteFileVersioned(input.Namespace, input.ProjectName, input.Path, input.Content, input.ExpectedVersion)
		if err != nil {
			var conflict *storage.VersionConflictError
			if errors.As(err, &conflict) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("version conflict: file is now at %s (you expected %s); re-read the file and retry with the current version", conflict.Current, conflict.Expected)}},
					IsError: true,
				}, WriteFileOutput{Conflict: true, CurrentVersion: conflict.Current}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to write file: %v", err)}},
				IsError: true,
			}, WriteFileOutput{}, nil
		}

		return nil, WriteFileOutput{Message: fmt.Sprintf("File %s written successfully", input.Path), Version: version}, nil
	}
}

type DeleteFileInput struct {
	Namespace       string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName     string `json:"project_name" jsonschema:"required, the name of the project"`
	Path            string `json:"path" jsonschema:"required, the path to the file to delete"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"optional, the commit hash the file is expected to be at; if set and stale, the delete is rejected with a version conflict"`
}

type DeleteFileOutput struct {
	Message        string `json:"message"`
	Version        string `json:"version,omitempty"`
	Conflict       bool   `json:"conflict,omitempty"`
	CurrentVersion string `json:"current_version,omitempty"`
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

		version, err := s.DeleteFileVersioned(input.Namespace, input.ProjectName, input.Path, input.ExpectedVersion)
		if err != nil {
			var conflict *storage.VersionConflictError
			if errors.As(err, &conflict) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("version conflict: file is now at %s (you expected %s); re-read the file and retry with the current version", conflict.Current, conflict.Expected)}},
					IsError: true,
				}, DeleteFileOutput{Conflict: true, CurrentVersion: conflict.Current}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to delete file: %v", err)}},
				IsError: true,
			}, DeleteFileOutput{}, nil
		}

		return nil, DeleteFileOutput{Message: fmt.Sprintf("File %s deleted successfully", input.Path), Version: version}, nil
	}
}

type GetHistoryInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path,omitempty" jsonschema:"optional, the path to the file to get history for (if empty, returns project history)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"optional, the maximum number of history entries to return (defaults to 10)"`
	Since       string `json:"since,omitempty" jsonschema:"optional, only commits after this RFC3339 timestamp or commit hash (exclusive)"`
}

// filterHistorySince keeps only commits after the given point, which may be an
// RFC3339 timestamp or a commit hash (exclusive).
func filterHistorySince(commits []storage.CommitInfo, since string) []storage.CommitInfo {
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		out := []storage.CommitInfo{}
		for _, c := range commits {
			if c.Date.After(t) {
				out = append(out, c)
			}
		}
		return out
	}
	out := []storage.CommitInfo{}
	for _, c := range commits {
		if c.Hash == since || strings.HasPrefix(c.Hash, since) {
			break
		}
		out = append(out, c)
	}
	return out
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
		limit := input.Limit
		if limit <= 0 {
			limit = 10
		}

		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, GetHistoryOutput{}, nil
		}

		// When 'since' is set, fetch the full history then filter and truncate so
		// the limit applies to the post-filter result.
		fetchLimit := limit
		if input.Since != "" {
			fetchLimit = 0
		}

		history, err := s.GetHistory(input.Namespace, input.ProjectName, input.Path, fetchLimit)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to get history: %v", err)}},
				IsError: true,
			}, GetHistoryOutput{}, nil
		}

		if input.Since != "" {
			history = filterHistorySince(history, input.Since)
			if len(history) > limit {
				history = history[:limit]
			}
		}

		return nil, GetHistoryOutput{History: history}, nil
	}
}

type ReadFileAtVersionInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
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
