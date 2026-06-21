package tools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/authz"
)

// AuthzMiddleware is the single authorization choke point for MCP tool calls (the
// B-28 stage-2 enforcement flip; originally the dormant 2026-06-15 foundation). It is
// installed once via Server.AddReceivingMiddleware — mirroring CoreFirstToolsMiddleware
// — so EVERY current and future tool flows through it without per-handler wiring.
//
// It no-ops for every method other than "tools/call". On a tools/call it reads the
// authenticated principal from the request context (propagated by the auth middleware,
// internal/auth.WithPrincipal) and the target namespace/project from the call
// arguments, looks up the tool's required level from toolLevels, and applies the
// shared authz.Authorize — the SAME decision function the WebUI /ws/ui gate calls.
//
// Today every production principal is super-user (DCR + self-issued tokens carry "*",
// the stage-1 first admin "*:admin"), so all current tools still pass; a scoped token
// (a later stage) is enforced by level. A denied call returns an IsError CallToolResult
// (Shoka's tool-error convention, matching LoggedTool's panic path), not a transport
// error.
func AuthzMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			name, namespace, project := callTarget(req)
			p, _ := auth.PrincipalFrom(ctx) // absent → zero Principal (Scope "") → super-user
			if err := authz.Authorize(p.Scope, namespace, project, toolLevel(name)); err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "unauthorized: " + err.Error()}},
				}, nil
			}
			return next(ctx, method, req)
		}
	}
}

// callTarget extracts the tool name and the target namespace/project from a
// tools/call request. The arguments are the raw wire JSON (not yet unmarshaled into
// the tool's typed input), so it decodes only the two routing fields by their uniform
// names (every tool input uses `namespace`/`project_name`; see metadataAttrs). A
// malformed/absent argument yields empty strings, which authz treats as a global op.
func callTarget(req mcp.Request) (name, namespace, project string) {
	ctr, ok := req.(*mcp.CallToolRequest)
	if !ok || ctr.Params == nil {
		return "", "", ""
	}
	name = ctr.Params.Name
	var args struct {
		Namespace   string `json:"namespace"`
		ProjectName string `json:"project_name"`
	}
	if len(ctr.Params.Arguments) > 0 {
		_ = json.Unmarshal(ctr.Params.Arguments, &args)
	}
	return name, args.Namespace, args.ProjectName
}

// toolLevels is the single registry mapping each MCP tool to the authorization level
// it requires. Read tools need read; content-mutating tools need write; the
// near-destructive recover_project needs admin (it was a latent gap — previously
// unclassified, so it mapped to read). A tool absent from this map fails CLOSED at
// admin, so a newly-added tool must be classified here before a non-super-user can
// reach it.
var toolLevels = map[string]authz.Level{
	// read
	"read_file":            authz.LevelRead,
	"read_summary":         authz.LevelRead,
	"read_file_at_version": authz.LevelRead,
	"list_files":           authz.LevelRead,
	"list_files_since":     authz.LevelRead,
	"list_projects":        authz.LevelRead, // global (no target namespace)
	"get_history":          authz.LevelRead,
	"get_diff":             authz.LevelRead,
	"get_server_info":      authz.LevelRead, // global
	"search_files":         authz.LevelRead,
	"subscribe":            authz.LevelRead,
	"unsubscribe":          authz.LevelRead,
	// write (the former mutatingTools set)
	"write_file":     authz.LevelWrite,
	"patch_file":     authz.LevelWrite,
	"append_to_file": authz.LevelWrite,
	"move_file":      authz.LevelWrite,
	"delete_file":    authz.LevelWrite,
	// admin
	"recover_project": authz.LevelAdmin,
	// B-28 deleted-file log (the 2026-06-18 directive): listing/reviving deleted files
	// is admin on the target namespace (the recover_project template). Revival is a
	// privileged, near-destructive-adjacent op; listing the deleted overlay is also
	// admin-gated so the enumeration surface stays admin-only.
	"list_deleted": authz.LevelAdmin,
	"revive_file":  authz.LevelAdmin,
	// admin on the target namespace (B-28 ns/proj management part 1): project
	// create/delete are namespace:admin (create RAISED from write — a write-only
	// principal can no longer create projects). The namespace ops carry admin here too,
	// but are SUPER-USER only — their handlers additionally enforce authz.IsSuperUser,
	// because the namespace-targeted middleware check (admin on the named namespace) is
	// not "super-user only" (a namespace-admin would satisfy it for its own namespace).
	"create_project":   authz.LevelAdmin,
	"delete_project":   authz.LevelAdmin,
	"create_namespace": authz.LevelAdmin,
	"delete_namespace": authz.LevelAdmin,
	// B-28 stage B: namespace health (read, global target ⇒ admin-somewhere; the handler
	// filters to the principal's admin namespaces) + recovery (admin on the target
	// namespace via callTarget; namespace-level actions tighten to super-user in the handler).
	"namespace_health":  authz.LevelAdmin,
	"namespace_recover": authz.LevelAdmin,
	// project move = admin on the target (the gate floor); the handler tightens to
	// super-user (admin-on-both is the designed-for future relaxation).
	"move_project": authz.LevelAdmin,
	// B-28 ns/proj rename: rename_project = admin on the namespace (the project stays in its
	// namespace — looser than move, no handler super-user tighten); rename_namespace = admin
	// gate floor, the handler tightens to super-user (it relabels the whole namespace).
	"rename_project":   authz.LevelAdmin,
	"rename_namespace": authz.LevelAdmin,
}

// toolLevel returns the required level for a tool, defaulting to admin (fail-closed)
// for any unregistered tool so a new tool cannot be silently world-reachable.
func toolLevel(name string) authz.Level {
	if l, ok := toolLevels[name]; ok {
		return l
	}
	return authz.LevelAdmin
}
