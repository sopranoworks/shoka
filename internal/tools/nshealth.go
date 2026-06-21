package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/utils"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/authz"
)

// Namespace health + recovery ops (B-28 ns/proj-management stage B). Narrow storage
// capabilities (the recover_project/projectRecoverer pattern) keep StorageService
// unwidened. *storage.FSGitStorage satisfies them.

type namespaceHealthReader interface {
	CheckAllHealth() storage.HealthReport
}

type namespaceRecoverer interface {
	DropMissingNamespace(namespace string) error
	DropMissingProject(namespace, projectName string) error
	CleanOrphanedSibling(namespace, name string) error
	AdoptForeign(namespace, projectName string) error
}

type NamespaceHealthInput struct{}

// NamespaceHealthOutput is the admin-filtered health picture.
type NamespaceHealthOutput struct {
	Report storage.HealthReport `json:"report"`
}

// NamespaceHealthHandler returns the managed-namespace health picture, filtered to the
// principal's admin scope: a super-user sees every namespace (and base-level foreign
// namespaces); a namespace-admin sees only the namespaces they administer (and no
// base-level foreign listing). The AuthzMiddleware already gated this at admin-somewhere
// (toolLevels admin + global target), so reaching here means the principal is at least one
// namespace's admin; the filter shapes the result (the filterProjectsByReadScope pattern).
func NamespaceHealthHandler(s namespaceHealthReader) func(context.Context, *mcp.CallToolRequest, NamespaceHealthInput) (*mcp.CallToolResult, NamespaceHealthOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ NamespaceHealthInput) (*mcp.CallToolResult, NamespaceHealthOutput, error) {
		p, _ := auth.PrincipalFrom(ctx)
		return nil, NamespaceHealthOutput{Report: filterHealthByAdminScope(s.CheckAllHealth(), p.Scope)}, nil
	}
}

// filterHealthByAdminScope narrows a health report to what the principal may see: a
// super-user keeps everything; a namespace-admin keeps only the namespaces it administers
// and drops the base-level foreign-namespace listing (a super-user-only view). Shared by
// the MCP and /ws/ui surfaces.
func filterHealthByAdminScope(report storage.HealthReport, scope string) storage.HealthReport {
	adminNs, superUser := authz.AdminNamespaces(scope)
	if superUser {
		return report
	}
	allow := make(map[string]bool, len(adminNs))
	for _, ns := range adminNs {
		allow[ns] = true
	}
	out := storage.HealthReport{}
	for _, nh := range report.Namespaces {
		if allow[nh.Name] {
			out.Namespaces = append(out.Namespaces, nh)
		}
	}
	// ForeignNamespaces (base-level dirs not managed) are a super-user-only view — omitted.
	return out
}

type NamespaceRecoverInput struct {
	// Action is the recovery action: "drop_missing" (drop a registry record whose on-disk
	// target is confirmed absent), "clean_orphaned" (remove a stray catalog/index .db with
	// no project dir), or "adopt" (bring a valid untracked namespace/project under
	// management).
	Action string `json:"action" jsonschema:"required, one of: drop_missing | clean_orphaned | adopt"`
	// Namespace is the target namespace (defaults to 'default').
	Namespace string `json:"namespace,omitempty" jsonschema:"optional, the target namespace (defaults to 'default')"`
	// ProjectName is the target project / stray name. Empty ⇒ a namespace-level action
	// (drop_missing / adopt of a whole namespace), which requires a super-user.
	ProjectName string `json:"project_name,omitempty" jsonschema:"optional, the target project (or stray name); empty targets the whole namespace (super-user only)"`
}

type NamespaceRecoverOutput struct {
	Message string `json:"message"`
}

// NamespaceRecoverHandler performs one explicit, non-destructive-by-default recovery
// action. authz: project-level actions need admin on the target namespace (enforced by the
// AuthzMiddleware via toolLevels + callTarget); namespace-level actions (empty project)
// are tightened to super-user here (the part-1 pattern: the middleware's namespace-targeted
// admin would let a namespace-admin through for its own namespace).
func NamespaceRecoverHandler(s namespaceRecoverer) func(context.Context, *mcp.CallToolRequest, NamespaceRecoverInput) (*mcp.CallToolResult, NamespaceRecoverOutput, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input NamespaceRecoverInput) (*mcp.CallToolResult, NamespaceRecoverOutput, error) {
		if input.Namespace == "" {
			input.Namespace = "default"
		}
		if !utils.IsValidName(input.Namespace) {
			return lifecycleErr("invalid namespace: only alphanumeric, hyphen, and underscore are allowed"), NamespaceRecoverOutput{}, nil
		}
		if input.ProjectName != "" && !utils.IsValidName(input.ProjectName) {
			return lifecycleErr("invalid project_name: only alphanumeric, hyphen, and underscore are allowed"), NamespaceRecoverOutput{}, nil
		}
		nsLevel := input.ProjectName == "" // a whole-namespace action
		ctx = notify.WithSender(ctx, mcpSender(req))

		switch input.Action {
		case "drop_missing":
			if nsLevel {
				if denied, ok := requireSuperUser(ctx); !ok {
					return denied, NamespaceRecoverOutput{}, nil
				}
				if err := s.DropMissingNamespace(input.Namespace); err != nil {
					return lifecycleErr(fmt.Sprintf("drop_missing namespace: %v", err)), NamespaceRecoverOutput{}, nil
				}
				return nil, NamespaceRecoverOutput{Message: fmt.Sprintf("Dropped missing namespace %s from the managed set", input.Namespace)}, nil
			}
			if err := s.DropMissingProject(input.Namespace, input.ProjectName); err != nil {
				return lifecycleErr(fmt.Sprintf("drop_missing project: %v", err)), NamespaceRecoverOutput{}, nil
			}
			return nil, NamespaceRecoverOutput{Message: fmt.Sprintf("Dropped missing project %s/%s from the managed set", input.Namespace, input.ProjectName)}, nil

		case "clean_orphaned":
			if nsLevel {
				return lifecycleErr("clean_orphaned requires project_name (the stray's base name)"), NamespaceRecoverOutput{}, nil
			}
			if err := s.CleanOrphanedSibling(input.Namespace, input.ProjectName); err != nil {
				return lifecycleErr(fmt.Sprintf("clean_orphaned: %v", err)), NamespaceRecoverOutput{}, nil
			}
			return nil, NamespaceRecoverOutput{Message: fmt.Sprintf("Cleaned orphaned siblings for %s/%s", input.Namespace, input.ProjectName)}, nil

		case "adopt":
			if nsLevel {
				if denied, ok := requireSuperUser(ctx); !ok {
					return denied, NamespaceRecoverOutput{}, nil
				}
			}
			if err := s.AdoptForeign(input.Namespace, input.ProjectName); err != nil {
				return lifecycleErr(fmt.Sprintf("adopt: %v", err)), NamespaceRecoverOutput{}, nil
			}
			target := input.Namespace
			if input.ProjectName != "" {
				target = input.Namespace + "/" + input.ProjectName
			}
			return nil, NamespaceRecoverOutput{Message: fmt.Sprintf("Adopted %s into the managed set", target)}, nil

		default:
			return lifecycleErr("invalid action: must be drop_missing | clean_orphaned | adopt"), NamespaceRecoverOutput{}, nil
		}
	}
}
