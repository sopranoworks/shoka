package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/authz"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/utils"
)

// The destructive + identity project/namespace ops (B-28 ns/proj management part 1).
// Each uses a NARROW storage capability interface (the recover_project/projectRecoverer
// pattern) so the broad StorageService surface stays unwidened. *storage.FSGitStorage
// satisfies them.

// projectDeleter is the storage capability behind delete_project: remove an entire
// project (dir + sibling catalog/index DBs) and cascade-clean its grants.
type projectDeleter interface {
	DeleteProject(ctx context.Context, namespace, projectName string) error
}

// namespaceManager is the storage capability behind create_namespace / delete_namespace.
type namespaceManager interface {
	CreateNamespace(namespace string) error
	DeleteNamespace(ctx context.Context, namespace string) error
}

// projectMover is the storage capability behind move_project (B-28 project move).
type projectMover interface {
	MoveProject(ctx context.Context, oldNamespace, projectName, newNamespace string) error
}

// projectRenamer / namespaceRenamer are the storage capabilities behind rename_project /
// rename_namespace (B-28 ns/proj rename).
type projectRenamer interface {
	RenameProject(ctx context.Context, namespace, oldName, newName string) error
}

type namespaceRenamer interface {
	RenameNamespace(ctx context.Context, oldName, newName string) error
}

// requireSuperUser returns an IsError result (and false) unless the request's principal
// is a super-user (wildcard admin). It is the authoritative super-user gate for the
// namespace ops — the AuthzMiddleware only verifies admin on the named namespace, which a
// namespace-admin would satisfy for its own namespace, so the namespace create/delete
// handlers tighten it to super-user here (the directive's "via the helper, not the loose
// empty-target form").
func requireSuperUser(ctx context.Context) (*mcp.CallToolResult, bool) {
	p, _ := auth.PrincipalFrom(ctx)
	if !authz.IsSuperUser(p.Scope) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "unauthorized: namespace management requires a super-user"}},
		}, false
	}
	return nil, true
}

type DeleteProjectInput struct {
	Namespace   string `json:"namespace,omitempty" jsonschema:"optional, the namespace for the project (defaults to 'default')"`
	ProjectName string `json:"project_name" jsonschema:"required, the name of the project to delete"`
}

type DeleteProjectOutput struct {
	Message string `json:"message"`
}

// DeleteProjectHandler permanently removes an entire project (its working tree, git repo,
// and both sibling derivative DBs) and cascade-cleans every grant that referenced it.
// authz: admin on the target namespace (enforced by the AuthzMiddleware via toolLevels).
func DeleteProjectHandler(s projectDeleter) func(context.Context, *mcp.CallToolRequest, DeleteProjectInput) (*mcp.CallToolResult, DeleteProjectOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input DeleteProjectInput) (*mcp.CallToolResult, DeleteProjectOutput, error) {
		if input.ProjectName == "" {
			return lifecycleErr("project_name is required"), DeleteProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) {
			return lifecycleErr("invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"), DeleteProjectOutput{}, nil
		}
		ctx = notify.WithSender(ctx, mcpSender(req))
		if err := s.DeleteProject(ctx, input.Namespace, input.ProjectName); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to delete project: %v", err)), DeleteProjectOutput{}, nil
		}
		return nil, DeleteProjectOutput{Message: fmt.Sprintf("Project %s/%s deleted", input.Namespace, input.ProjectName)}, nil
	}
}

type CreateNamespaceInput struct {
	Namespace string `json:"namespace" jsonschema:"required, the name of the namespace to create"`
}

type CreateNamespaceOutput struct {
	Message string `json:"message"`
}

// CreateNamespaceHandler creates an explicit, empty namespace. authz: SUPER-USER only
// (verified here via authz.IsSuperUser, beyond the middleware's namespace-targeted admin).
func CreateNamespaceHandler(s namespaceManager) func(context.Context, *mcp.CallToolRequest, CreateNamespaceInput) (*mcp.CallToolResult, CreateNamespaceOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input CreateNamespaceInput) (*mcp.CallToolResult, CreateNamespaceOutput, error) {
		if input.Namespace == "" {
			return lifecycleErr("namespace is required"), CreateNamespaceOutput{}, nil
		}
		if !utils.IsValidName(input.Namespace) {
			return lifecycleErr("invalid namespace: only alphanumeric, hyphen, and underscore are allowed"), CreateNamespaceOutput{}, nil
		}
		if denied, ok := requireSuperUser(ctx); !ok {
			return denied, CreateNamespaceOutput{}, nil
		}
		if err := s.CreateNamespace(input.Namespace); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to create namespace: %v", err)), CreateNamespaceOutput{}, nil
		}
		return nil, CreateNamespaceOutput{Message: fmt.Sprintf("Namespace %s created", input.Namespace)}, nil
	}
}

type DeleteNamespaceInput struct {
	Namespace string `json:"namespace" jsonschema:"required, the name of the namespace to delete (removes ALL its projects)"`
}

type DeleteNamespaceOutput struct {
	Message string `json:"message"`
}

// DeleteNamespaceHandler permanently removes a namespace and every project under it, and
// cascade-cleans every grant that referenced it. authz: SUPER-USER only (verified here).
func DeleteNamespaceHandler(s namespaceManager) func(context.Context, *mcp.CallToolRequest, DeleteNamespaceInput) (*mcp.CallToolResult, DeleteNamespaceOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input DeleteNamespaceInput) (*mcp.CallToolResult, DeleteNamespaceOutput, error) {
		if input.Namespace == "" {
			return lifecycleErr("namespace is required"), DeleteNamespaceOutput{}, nil
		}
		if !utils.IsValidName(input.Namespace) {
			return lifecycleErr("invalid namespace: only alphanumeric, hyphen, and underscore are allowed"), DeleteNamespaceOutput{}, nil
		}
		if denied, ok := requireSuperUser(ctx); !ok {
			return denied, DeleteNamespaceOutput{}, nil
		}
		ctx = notify.WithSender(ctx, mcpSender(req))
		if err := s.DeleteNamespace(ctx, input.Namespace); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to delete namespace: %v", err)), DeleteNamespaceOutput{}, nil
		}
		return nil, DeleteNamespaceOutput{Message: fmt.Sprintf("Namespace %s deleted", input.Namespace)}, nil
	}
}

type MoveProjectInput struct {
	Namespace    string `json:"namespace,omitempty" jsonschema:"optional, the SOURCE namespace (defaults to 'default')"`
	ProjectName  string `json:"project_name" jsonschema:"required, the project to move"`
	NewNamespace string `json:"new_namespace" jsonschema:"required, the TARGET namespace (must already exist)"`
}

type MoveProjectOutput struct {
	Message string `json:"message"`
}

// MoveProjectHandler relocates a project from one namespace to another (B-28 project move).
// authz: SUPER-USER only in this first cut (verified here). A future relaxation would replace
// requireSuperUser with the handler dual-check Authorize(scope, oldNs, project, LevelAdmin)
// AND Authorize(scope, newNs, "", LevelAdmin) — admin on both — with no authz model change.
func MoveProjectHandler(s projectMover) func(context.Context, *mcp.CallToolRequest, MoveProjectInput) (*mcp.CallToolResult, MoveProjectOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input MoveProjectInput) (*mcp.CallToolResult, MoveProjectOutput, error) {
		if input.ProjectName == "" || input.NewNamespace == "" {
			return lifecycleErr("project_name and new_namespace are required"), MoveProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) || !utils.IsValidName(input.NewNamespace) {
			return lifecycleErr("invalid namespace or project_name: only alphanumeric, hyphen, and underscore are allowed"), MoveProjectOutput{}, nil
		}
		if denied, ok := requireSuperUser(ctx); !ok {
			return denied, MoveProjectOutput{}, nil
		}
		ctx = notify.WithSender(ctx, mcpSender(req))
		if err := s.MoveProject(ctx, input.Namespace, input.ProjectName, input.NewNamespace); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to move project: %v", err)), MoveProjectOutput{}, nil
		}
		return nil, MoveProjectOutput{Message: fmt.Sprintf("Moved project %s → %s/%s", input.Namespace+"/"+input.ProjectName, input.NewNamespace, input.ProjectName)}, nil
	}
}

type RenameProjectInput struct {
	Namespace      string `json:"namespace,omitempty" jsonschema:"optional, the namespace of the project (defaults to 'default')"`
	ProjectName    string `json:"project_name" jsonschema:"required, the current name of the project to rename"`
	NewProjectName string `json:"new_project_name" jsonschema:"required, the new project name (must be free in the namespace)"`
}

type RenameProjectOutput struct {
	Message string `json:"message"`
}

// RenameProjectHandler renames a project within its namespace (B-28 ns/proj rename). authz:
// admin on the namespace (enforced by the AuthzMiddleware via toolLevels) — NOT super-user, as
// the project does not leave its namespace (looser than move).
func RenameProjectHandler(s projectRenamer) func(context.Context, *mcp.CallToolRequest, RenameProjectInput) (*mcp.CallToolResult, RenameProjectOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input RenameProjectInput) (*mcp.CallToolResult, RenameProjectOutput, error) {
		if input.ProjectName == "" || input.NewProjectName == "" {
			return lifecycleErr("project_name and new_project_name are required"), RenameProjectOutput{}, nil
		}
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.ProjectName) || !utils.IsValidName(input.NewProjectName) {
			return lifecycleErr("invalid namespace or project name: only alphanumeric, hyphen, and underscore are allowed"), RenameProjectOutput{}, nil
		}
		ctx = notify.WithSender(ctx, mcpSender(req))
		if err := s.RenameProject(ctx, input.Namespace, input.ProjectName, input.NewProjectName); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to rename project: %v", err)), RenameProjectOutput{}, nil
		}
		return nil, RenameProjectOutput{Message: fmt.Sprintf("Renamed project %s/%s → %s/%s", input.Namespace, input.ProjectName, input.Namespace, input.NewProjectName)}, nil
	}
}

type RenameNamespaceInput struct {
	Namespace    string `json:"namespace" jsonschema:"required, the current name of the namespace to rename"`
	NewNamespace string `json:"new_namespace" jsonschema:"required, the new namespace name (must be free)"`
}

type RenameNamespaceOutput struct {
	Message string `json:"message"`
}

// RenameNamespaceHandler relabels a whole namespace (B-28 ns/proj rename). authz: SUPER-USER
// only (verified here), like create/delete-namespace — it relabels the whole namespace and all
// its grants. The `default` namespace is rename-protected (refused in storage).
func RenameNamespaceHandler(s namespaceRenamer) func(context.Context, *mcp.CallToolRequest, RenameNamespaceInput) (*mcp.CallToolResult, RenameNamespaceOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input RenameNamespaceInput) (*mcp.CallToolResult, RenameNamespaceOutput, error) {
		if input.Namespace == "" || input.NewNamespace == "" {
			return lifecycleErr("namespace and new_namespace are required"), RenameNamespaceOutput{}, nil
		}
		if !utils.IsValidName(input.Namespace) || !utils.IsValidName(input.NewNamespace) {
			return lifecycleErr("invalid namespace: only alphanumeric, hyphen, and underscore are allowed"), RenameNamespaceOutput{}, nil
		}
		if denied, ok := requireSuperUser(ctx); !ok {
			return denied, RenameNamespaceOutput{}, nil
		}
		ctx = notify.WithSender(ctx, mcpSender(req))
		if err := s.RenameNamespace(ctx, input.Namespace, input.NewNamespace); err != nil {
			return lifecycleErr(fmt.Sprintf("failed to rename namespace: %v", err)), RenameNamespaceOutput{}, nil
		}
		return nil, RenameNamespaceOutput{Message: fmt.Sprintf("Renamed namespace %s → %s", input.Namespace, input.NewNamespace)}, nil
	}
}

// errResult is the shared IsError CallToolResult builder for this file's handlers.
func lifecycleErr(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}
