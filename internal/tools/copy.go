package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// etagAbsent is the SHA-256 of nil/empty bytes — the etag that writeTransformed
// computes when os.ReadFile returns os.ErrNotExist (current = nil). Passing it as
// ifMatch atomically asserts the destination does not exist inside the per-file lock.
var etagAbsent = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

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

		// Fast-path existence check (outside the lock — cheap rejection when
		// the destination already exists without needing to read the source first
		// on the slow path). The authoritative guard is the ifMatch=etagAbsent
		// passed to Write below, which atomically asserts absence inside the
		// per-file lock, closing the TOCTOU window.
		if _, _, rerr := s.ReadFileWithETag(input.Namespace, input.ProjectName, destPath); rerr == nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("destination file already exists: %s", destPath)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		} else if !strings.Contains(rerr.Error(), "no such file") && !strings.Contains(rerr.Error(), "not exist") {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to check destination: %v", rerr)}},
				IsError: true,
			}, CopyFileOutput{}, nil
		}

		// Write to destination with ifMatch=etagAbsent: inside the per-file lock
		// this asserts the file still does not exist (sha256 of nil bytes). A
		// concurrent writer that lands between our check above and the lock
		// acquisition triggers a VersionConflictError — no silent overwrite.
		ctx = withWriteIdentity(ctx, req)
		ctx = notify.WithSender(ctx, mcpSender(req))
		etag, err := s.Write(ctx, "", input.Namespace, input.ProjectName, destPath, content, &etagAbsent)
		if err != nil {
			var conflict *storage.VersionConflictError
			if errors.As(err, &conflict) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("destination file already exists: %s", destPath)}},
					IsError: true,
				}, CopyFileOutput{}, nil
			}
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
