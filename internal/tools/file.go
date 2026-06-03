package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

// workerMetaKey is the Shoka-namespaced key under the MCP initialize _meta where
// a client may declare its Rohrpost worker id.
const workerMetaKey = "shoka/worker-id"

// mcpSender derives the notify sender identity for an MCP write (the 2026-06-01
// sender-exclusion directive). It is the MCP session id, prefixed "mcp:" so it
// can never collide with a /ws/ui connection id ("ws-<seq>") — guaranteeing an
// MCP write reaches every /ws/ui subscriber (none of them is its originator). It
// is also session-stable, so a future MCP-side subscriber would correctly be
// excluded from its own writes. Falls back to a constant "mcp" when no session
// is present (e.g. in tests): still non-colliding, so delivery is unaffected.
func mcpSender(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return "mcp"
	}
	return "mcp:" + req.Session.ID()
}

// agentFromMCP extracts a connecting agent's self-declared identity from the MCP
// session — the client name from initialize clientInfo, and an optional worker
// id from the initialize _meta (the protocol's reserved metadata slot). Both are
// native MCP and additive; a client that declares nothing yields a zero Agent,
// and the configured default applies downstream (internal/identity.Resolve).
func agentFromMCP(req *mcp.CallToolRequest) identity.Agent {
	var a identity.Agent
	if req == nil || req.Session == nil {
		return a
	}
	ip := req.Session.InitializeParams()
	if ip == nil {
		return a
	}
	if ip.ClientInfo != nil {
		a.Name = ip.ClientInfo.Name
	}
	if w, ok := ip.Meta[workerMetaKey].(string); ok {
		a.Worker = w
	}
	return a
}

type ReadFileInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string `json:"path" jsonschema:"required, the path to the file to read"`
}

type ReadFileOutput struct {
	Content string `json:"content"`
	// ETag is an opaque token (the SHA-256 of the content). Pass it as if_match on
	// a subsequent write_file/delete_file to assert the file has not changed. It
	// is NOT a git commit hash and is not valid input to read_file_at_version.
	ETag string `json:"etag"`
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

		content, etag, err := s.ReadFileWithETag(input.Namespace, input.ProjectName, input.Path)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to read file: %v", err)}},
				IsError: true,
			}, ReadFileOutput{}, nil
		}

		return nil, ReadFileOutput{Content: content, ETag: etag}, nil
	}
}

type WriteFileInput struct {
	Namespace   string  `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string  `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string  `json:"path" jsonschema:"required, the path to the file to write"`
	Content     string  `json:"content" jsonschema:"required, the content to write to the file"`
	IfMatch     *string `json:"if_match,omitempty" jsonschema:"optional, the etag the file is expected to be at (from read_file); if set and stale, the write is rejected with a conflict and no change is made"`
}

type WriteFileOutput struct {
	Message string `json:"message,omitempty"`
	// ETag is the new etag (SHA-256 of the written content) on success.
	ETag string `json:"etag,omitempty"`
	// Conflict is true when if_match did not match; CurrentETag then holds the
	// file's actual current etag.
	Conflict    bool   `json:"conflict,omitempty"`
	CurrentETag string `json:"current_etag,omitempty"`
	// Reason is set on a project-state refusal: "corrupted" | "dangerous" | "write_disabled".
	Reason string `json:"reason,omitempty"`
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

		ctx = identity.WithAgent(ctx, agentFromMCP(req))
		ctx = notify.WithSender(ctx, mcpSender(req))
		etag, err := s.Write(ctx, "", input.Namespace, input.ProjectName, input.Path, input.Content, input.IfMatch)
		if err != nil {
			if text, conflict, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: text}},
						IsError: true,
					}, WriteFileOutput{
						Conflict:    conflict != "",
						CurrentETag: conflict,
						Reason:      reason,
					}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to write file: %v", err)}},
				IsError: true,
			}, WriteFileOutput{}, nil
		}

		return nil, WriteFileOutput{Message: fmt.Sprintf("File %s written successfully", input.Path), ETag: etag}, nil
	}
}

// classifyWriteErr maps a storage write/delete error to a user message plus the
// structured fields for conflict (current etag) or project-state (reason). ok is
// false for an unrecognised error.
func classifyWriteErr(err error) (text, currentETag, reason string, ok bool) {
	var conflict *storage.VersionConflictError
	if errors.As(err, &conflict) {
		return fmt.Sprintf("etag conflict: file is now at %s (you sent if_match %s); re-read the file and retry with the current etag", conflict.Current, conflict.Expected),
			conflict.Current, "", true
	}
	switch {
	case errors.Is(err, storage.ErrProjectDangerous):
		return "project is in a dangerous state (git repository unreadable); recover it before writing", "", "dangerous", true
	case errors.Is(err, storage.ErrProjectCorrupted):
		return "project is in a corrupted state (uncommitted working-tree drift); recover it before writing", "", "corrupted", true
	case errors.Is(err, storage.ErrWriteDisabled):
		return "writes are temporarily disabled (write-ahead log is full); retry once it drains", "", "write_disabled", true
	}
	return "", "", "", false
}

type DeleteFileInput struct {
	Namespace   string  `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string  `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string  `json:"path" jsonschema:"required, the path to the file to delete"`
	IfMatch     *string `json:"if_match,omitempty" jsonschema:"optional, the etag the file is expected to be at; if set and stale, the delete is rejected with a conflict"`
}

type DeleteFileOutput struct {
	Message     string `json:"message,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
	CurrentETag string `json:"current_etag,omitempty"`
	Reason      string `json:"reason,omitempty"`
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

		ctx = identity.WithAgent(ctx, agentFromMCP(req))
		ctx = notify.WithSender(ctx, mcpSender(req))
		err := s.Delete(ctx, "", input.Namespace, input.ProjectName, input.Path, input.IfMatch)
		if err != nil {
			if text, conflict, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: text}},
						IsError: true,
					}, DeleteFileOutput{
						Conflict:    conflict != "",
						CurrentETag: conflict,
						Reason:      reason,
					}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to delete file: %v", err)}},
				IsError: true,
			}, DeleteFileOutput{}, nil
		}

		return nil, DeleteFileOutput{Message: fmt.Sprintf("File %s deleted successfully", input.Path)}, nil
	}
}

type MoveFileInput struct {
	Namespace   string  `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string  `json:"project_name" jsonschema:"required, the name of the project"`
	SourcePath  string  `json:"source_path" jsonschema:"required, the current path of the file to move"`
	TargetPath  string  `json:"target_path" jsonschema:"required, the new path for the file (same project)"`
	IfMatch     *string `json:"if_match,omitempty" jsonschema:"optional, an etag for optimistic concurrency: validates the target's etag when the target already exists (explicit overwrite), otherwise the source's etag; a move never silently overwrites an existing target"`
}

type MoveFileOutput struct {
	Message string `json:"message,omitempty"`
	// NewETag is the destination's etag (SHA-256 of the moved content) on success.
	NewETag string `json:"new_etag,omitempty"`
	// LinksRewritten is the number of internal markdown links updated to point at
	// the new path. Link auto-update on move is currently DISABLED (backlog B-33,
	// 2026-06-03), so this is ALWAYS 0; the field is retained in the shape (no
	// omitempty — always emitted as 0) so re-enabling it later is additive, not a
	// contract change. See storage.rewriteInboundLinksForMove (re-enablement seam).
	LinksRewritten int `json:"links_rewritten"`
	// Conflict is true when if_match did not match (or the target exists and no
	// if_match was given); CurrentETag then holds the relevant file's current etag.
	Conflict    bool   `json:"conflict,omitempty"`
	CurrentETag string `json:"current_etag,omitempty"`
	// Reason is set on a project-state refusal: "corrupted" | "dangerous" | "write_disabled".
	Reason string `json:"reason,omitempty"`
}

func MoveFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, MoveFileInput) (*mcp.CallToolResult, MoveFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input MoveFileInput) (*mcp.CallToolResult, MoveFileOutput, error) {
		if input.ProjectName == "" || input.SourcePath == "" || input.TargetPath == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name, source_path, and target_path are required"}},
				IsError: true,
			}, MoveFileOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, MoveFileOutput{}, nil
		}

		ctx = identity.WithAgent(ctx, agentFromMCP(req))
		ctx = notify.WithSender(ctx, mcpSender(req))
		newEtag, links, err := s.Move(ctx, "", input.Namespace, input.ProjectName, input.SourcePath, input.TargetPath, input.IfMatch)
		if err != nil {
			if text, conflict, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: text}},
						IsError: true,
					}, MoveFileOutput{
						Conflict:    conflict != "",
						CurrentETag: conflict,
						Reason:      reason,
					}, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to move file: %v", err)}},
				IsError: true,
			}, MoveFileOutput{}, nil
		}

		return nil, MoveFileOutput{
			Message:        fmt.Sprintf("File moved from %s to %s", input.SourcePath, input.TargetPath),
			NewETag:        newEtag,
			LinksRewritten: links,
		}, nil
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
