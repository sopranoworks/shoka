package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
)

// list_deleted and revive_file are the admin-only deleted-file ops (the 2026-06-18
// deleted-log directive). list_deleted is a cheap O(cap) read of the per-project
// deleted-file log (no git walk); revive_file re-creates a deleted file
// forward-only from the parent of its deletion commit. Both are LevelAdmin,
// namespace-scoped (see internal/tools/authz.go toolLevels) — the recover_project
// authz template.

type ListDeletedInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
}

// DeletedFileOut is one currently-deleted file in the list_deleted response.
type DeletedFileOut struct {
	Path           string `json:"path"`
	DeletionCommit string `json:"deletion_commit"`
	DeletedAt      string `json:"deleted_at"`
}

type ListDeletedOutput struct {
	Deleted []DeletedFileOut `json:"deleted"`
}

// ListDeletedHandler lists a project's currently-deleted files. Admin-only.
func ListDeletedHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ListDeletedInput) (*mcp.CallToolResult, ListDeletedOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListDeletedInput) (*mcp.CallToolResult, ListDeletedOutput, error) {
		if input.ProjectName == "" {
			return errResult(&ListDeletedOutput{}, "project_name is required")
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return errResult(&ListDeletedOutput{}, "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed")
		}
		recs, err := s.ListDeleted(input.Namespace, input.ProjectName)
		if err != nil {
			return errResult(&ListDeletedOutput{}, fmt.Sprintf("failed to list deleted files: %v", err))
		}
		out := ListDeletedOutput{Deleted: make([]DeletedFileOut, 0, len(recs))}
		for _, r := range recs {
			out.Deleted = append(out.Deleted, DeletedFileOut{
				Path:           r.Path,
				DeletionCommit: r.DeletionCommit,
				DeletedAt:      r.DeletedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			})
		}
		return nil, out, nil
	}
}

type ReviveFileInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the within-project path of the deleted file to revive"`
	FromCommit  string `json:"from_commit,omitempty" jsonschema:"optional, override the deletion commit hash; when omitted the recorded deletion commit (or a name-specified history lookup) is used"`
}

type ReviveFileOutput struct {
	Revived bool   `json:"revived"`
	Path    string `json:"path"`
}

// ReviveFileHandler re-creates a deleted file forward-only as a new commit.
// Admin-only. A divergence (the deletion commit is gone from git) surfaces as an
// error result, not a silent failure.
func ReviveFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, ReviveFileInput) (*mcp.CallToolResult, ReviveFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ReviveFileInput) (*mcp.CallToolResult, ReviveFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" {
			return errResult(&ReviveFileOutput{}, "project_name and path are required")
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return errResult(&ReviveFileOutput{}, "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed")
		}
		if err := s.ReviveFile(ctx, input.Namespace, input.ProjectName, input.Path, input.FromCommit); err != nil {
			return errResult(&ReviveFileOutput{}, fmt.Sprintf("failed to revive file: %v", err))
		}
		return nil, ReviveFileOutput{Revived: true, Path: input.Path}, nil
	}
}
