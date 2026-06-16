package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
)

// projectRecoverer is the narrow storage capability recover_project needs: re-derive
// a project's state against the live on-disk git HEAD and re-sync its catalog. The
// concrete *storage.FSGitStorage satisfies it; a narrow interface keeps the tool off
// the broad StorageService surface (the WALInfoProvider/projectResolver pattern).
type projectRecoverer interface {
	ResyncToHead(namespace, projectName string) (storage.ProjectState, error)
}

type RecoverProjectInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project to recover"`
}

type RecoverProjectOutput struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	State     string `json:"state"`     // resulting state: healthy | corrupted | dangerous
	Recovered bool   `json:"recovered"` // true iff the project is now healthy and writable
	Message   string `json:"message"`
}

// RecoverProjectHandler re-syncs a project's write-path baseline to the ACTUAL
// on-disk git HEAD and clears a FALSE `corrupted` flag — the in-product recovery for
// a project an external HEAD move (a host `git reset`, an out-of-band landing)
// stranded as unwritable. It is non-destructive: it neither commits nor discards
// working-tree content. A clean-on-disk project is restored to healthy; a project
// with GENUINE uncommitted drift still reports corrupted, and the message points the
// operator at the destructive recover modes (accept-working-tree / accept-head)
// exposed by the Web UI recover action for that case.
func RecoverProjectHandler(s projectRecoverer) func(context.Context, *mcp.CallToolRequest, RecoverProjectInput) (*mcp.CallToolResult, RecoverProjectOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input RecoverProjectInput) (*mcp.CallToolResult, RecoverProjectOutput, error) {
		if input.ProjectName == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "project_name is required"}},
				IsError: true,
			}, RecoverProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"}},
				IsError: true,
			}, RecoverProjectOutput{}, nil
		}

		state, err := s.ResyncToHead(input.Namespace, input.ProjectName)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("recover_project failed: %v", err)}},
				IsError: true,
			}, RecoverProjectOutput{}, nil
		}

		out := RecoverProjectOutput{
			Namespace: input.Namespace,
			Project:   input.ProjectName,
			State:     string(state),
			Recovered: state == storage.StateHealthy,
		}
		switch state {
		case storage.StateHealthy:
			out.Message = fmt.Sprintf("Project %s/%s re-synced to the on-disk git HEAD and is healthy; writes are enabled.", input.Namespace, input.ProjectName)
		case storage.StateCorrupted:
			out.Message = fmt.Sprintf("Project %s/%s has GENUINE uncommitted working-tree drift (not a stale baseline); it remains corrupted. Resolve it with the Web UI recover action — 'accept-working-tree' to adopt the changes or 'accept-head' to discard them.", input.Namespace, input.ProjectName)
		case storage.StateDangerous:
			out.Message = fmt.Sprintf("Project %s/%s is in a dangerous state (its .git is unreadable or absent); recovery cannot proceed over MCP.", input.Namespace, input.ProjectName)
		default:
			out.Message = fmt.Sprintf("Project %s/%s state: %s.", input.Namespace, input.ProjectName, state)
		}
		return nil, out, nil
	}
}
