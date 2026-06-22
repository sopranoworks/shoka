package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
)

// Partial-edit MCP tools (backlog B-36): append_to_file and patch_file let an
// agent change a large append-mostly file by sending only the changed span. They
// mirror write_file's handler shape exactly — same identity/sender ctx wiring,
// same if_match conflict + project-state mapping via classifyWriteErr — and add
// one branch for the partial-edit-specific typed errors (a non-unique anchor /
// old_string, or a bad argument combination). Integrity is if_match only; there
// is deliberately no sha256 argument (see the directive §2.4 / the contract).

// classifyPartialEditErr maps the partial-edit-specific typed errors (a
// *storage.MatchError for a zero/ambiguous anchor or old_string, or one of the
// argument-combination sentinels) to a user-facing message. ok is false for any
// other error, so the caller falls through to classifyWriteErr (conflict /
// project-state) and finally a generic failure.
func classifyPartialEditErr(err error) (text string, ok bool) {
	var me *storage.MatchError
	if errors.As(err, &me) {
		return me.Error(), true
	}
	switch {
	case errors.Is(err, storage.ErrFileNotFound):
		return "file not found: append_to_file edits an existing file — use write_file to create it", true
	case errors.Is(err, storage.ErrEmptyOldString):
		return "old_string must not be empty", true
	case errors.Is(err, storage.ErrAnchorRequired):
		return "anchor is required when position is 'before' or 'after'", true
	case errors.Is(err, storage.ErrAnchorWithEnd):
		return "anchor must not be set when position is 'end'", true
	case errors.Is(err, storage.ErrInvalidPosition):
		return "invalid position: must be 'end', 'before', or 'after'", true
	}
	return "", false
}

type AppendToFileInput struct {
	Namespace   string  `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string  `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string  `json:"path" jsonschema:"required, the path to the file to append to"`
	Content     string  `json:"content" jsonschema:"required, the text to insert verbatim (the server adds no newline; you control newlines)"`
	Position    string  `json:"position,omitempty" jsonschema:"optional, where to insert: 'end' (default) appends at end of file; 'before' or 'after' insert relative to anchor"`
	Anchor      string  `json:"anchor,omitempty" jsonschema:"required when position is 'before' or 'after': a substring that must occur EXACTLY ONCE in the file; content is inserted immediately before/after it. Must be empty when position is 'end'. Zero or multiple matches are an error (the server never guesses)"`
	IfMatch     *string `json:"if_match,omitempty" jsonschema:"optional, the etag the file is expected to be at (from read_file); if set and stale, the append is rejected with a conflict and no change is made"`
}

type AppendToFileOutput struct {
	Message string `json:"message,omitempty"`
	// ETag is the new etag (SHA-256 of the whole file after the insert) on success.
	ETag string `json:"etag,omitempty"`
	// Conflict is true when if_match did not match; CurrentETag then holds the
	// file's actual current etag.
	Conflict    bool   `json:"conflict,omitempty"`
	CurrentETag string `json:"current_etag,omitempty"`
	// Reason is set on a project-state refusal: "corrupted" | "dangerous" | "write_disabled".
	Reason string `json:"reason,omitempty"`
}

func AppendToFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, AppendToFileInput) (*mcp.CallToolResult, AppendToFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input AppendToFileInput) (*mcp.CallToolResult, AppendToFileOutput, error) {
		if input.ProjectName == "" || input.Path == "" || input.Content == "" {
			return errResult(&AppendToFileOutput{}, "project_name, path, and content are required")
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return errResult(&AppendToFileOutput{}, "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed")
		}

		ctx = identity.WithAgent(ctx, agentFromMCP(req))
		ctx = notify.WithSender(ctx, mcpSender(req))
		etag, err := s.AppendToFile(ctx, "", input.Namespace, input.ProjectName, input.Path, input.Content, input.Position, input.Anchor, input.IfMatch)
		if err != nil {
			if text, ok := classifyPartialEditErr(err); ok {
				return errResult(&AppendToFileOutput{}, text)
			}
			if text, conflict, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: text}},
						IsError: true,
					}, AppendToFileOutput{
						Conflict:    conflict != "",
						CurrentETag: conflict,
						Reason:      reason,
					}, nil
			}
			return errResult(&AppendToFileOutput{}, fmt.Sprintf("failed to append to file: %v", err))
		}
		return nil, AppendToFileOutput{Message: fmt.Sprintf("Appended to %s", input.Path), ETag: etag}, nil
	}
}

type PatchFileInput struct {
	Namespace   string  `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string  `json:"project_name" jsonschema:"required, the name of the project"`
	Path        string  `json:"path" jsonschema:"required, the path to the file to patch"`
	OldString   string  `json:"old_string" jsonschema:"required, the exact text to replace; it must occur EXACTLY ONCE in the file. Include enough surrounding context to make it unique. Zero or multiple matches are an error (the server never guesses)"`
	NewString   string  `json:"new_string" jsonschema:"required, the replacement text (may be empty to delete the matched span)"`
	IfMatch     *string `json:"if_match,omitempty" jsonschema:"optional, the etag the file is expected to be at (from read_file); if set and stale, the patch is rejected with a conflict and no change is made"`
}

type PatchFileOutput struct {
	Message string `json:"message,omitempty"`
	// ETag is the new etag (SHA-256 of the whole file after the replace) on success.
	ETag        string `json:"etag,omitempty"`
	Conflict    bool   `json:"conflict,omitempty"`
	CurrentETag string `json:"current_etag,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func PatchFileHandler(s storage.StorageService) func(context.Context, *mcp.CallToolRequest, PatchFileInput) (*mcp.CallToolResult, PatchFileOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input PatchFileInput) (*mcp.CallToolResult, PatchFileOutput, error) {
		// new_string may be empty (a deletion), so it is not required; old_string is.
		if input.ProjectName == "" || input.Path == "" || input.OldString == "" {
			return errResult(&PatchFileOutput{}, "project_name, path, and old_string are required")
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return errResult(&PatchFileOutput{}, "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed")
		}

		ctx = identity.WithAgent(ctx, agentFromMCP(req))
		ctx = notify.WithSender(ctx, mcpSender(req))
		etag, err := s.PatchFile(ctx, "", input.Namespace, input.ProjectName, input.Path, input.OldString, input.NewString, input.IfMatch)
		if err != nil {
			if text, ok := classifyPartialEditErr(err); ok {
				return errResult(&PatchFileOutput{}, text)
			}
			if text, conflict, reason, ok := classifyWriteErr(err); ok {
				return &mcp.CallToolResult{
						Content: []mcp.Content{&mcp.TextContent{Text: text}},
						IsError: true,
					}, PatchFileOutput{
						Conflict:    conflict != "",
						CurrentETag: conflict,
						Reason:      reason,
					}, nil
			}
			return errResult(&PatchFileOutput{}, fmt.Sprintf("failed to patch file: %v", err))
		}
		return nil, PatchFileOutput{Message: fmt.Sprintf("Patched %s", input.Path), ETag: etag}, nil
	}
}

// errResult returns an isError CallToolResult carrying text, plus the zero-valued
// typed output. It keeps the validation/typed-error branches above terse while
// preserving each handler's structured output type.
func errResult[T any](out *T, text string) (*mcp.CallToolResult, T, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: true,
	}, *out, nil
}
