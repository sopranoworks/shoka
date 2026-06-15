package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/auth"
)

// AuthzMiddleware is the single authorization choke point for MCP tool calls (the
// 2026-06-15 authz foundation). It is installed once via Server.AddReceivingMiddleware
// — mirroring CoreFirstToolsMiddleware — so EVERY current and future tool flows
// through it without per-handler wiring (the operator's anti-scatter requirement).
//
// It no-ops for every method other than "tools/call". On a tools/call it reads the
// authenticated principal from the request context (already propagated there by the
// auth middleware — internal/auth.WithPrincipal at the HTTP layer) and the target
// namespace/project from the call arguments, and applies authorize().
//
// Today the gate is *-pass: every DCR token carries Scope "*", and an
// unauthenticated/plain-transport call carries no principal (Scope ""), both of
// which authorize() allows — so behaviour is unchanged. The else-branch (a non-"*"
// scope, e.g. a future pre-issued "namespace:foo" token) is DORMANT but fully
// implemented and tested, so enforcement becomes automatic the moment a scoped
// token exists, with no further change to the gate.
//
// A denied call returns an IsError CallToolResult (Shoka's tool-error convention,
// matching LoggedTool's panic path) rather than a transport error, so the client
// sees an ordinary tool failure.
func AuthzMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			name, namespace, project := callTarget(req)
			p, _ := auth.PrincipalFrom(ctx) // absent → zero Principal (Scope ""), allowed
			if err := authorize(p, namespace, project, actionFor(name)); err != nil {
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
// the tool's typed input), so it decodes only the two routing fields by their
// uniform names (every tool input uses `namespace`/`project_name`; see
// metadataAttrs). A malformed/absent argument yields empty strings, which
// authorize() treats conservatively.
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

// mutatingTools is the set of tools that change project content. actionFor maps a
// tool name to "write" for these and "read" otherwise. The action is carried into
// authorize() so a future finer policy can distinguish read from write grants; the
// current namespace-scope check does not branch on it.
var mutatingTools = map[string]bool{
	"create_project": true,
	"write_file":     true,
	"delete_file":    true,
	"append_to_file": true,
	"patch_file":     true,
	"move_file":      true,
	"translate_file": true,
}

func actionFor(toolName string) string {
	if mutatingTools[toolName] {
		return "write"
	}
	return "read"
}

// authorize is the authz decision for one tool call. It is REAL logic, not a
// no-op:
//
//   - Scope "*" or "" (the DCR all-access token, and the no-principal/plain-transport
//     case) ⇒ ALLOW. This is every token today, so behaviour is unchanged.
//   - A non-"*" scope ⇒ the request's namespace must match one of the scope's grants.
//     A grant is "*", "namespace:<ns>" (any project in that namespace), or
//     "namespace:<ns>/<project>" (that project only); grants are comma-separated.
//     A non-"*" scope with an EMPTY request namespace is denied (a scoped token only
//     grants the namespaces it names; refining the default-namespace case belongs to
//     the deferred per-namespace-enforcement leg).
//
// action is reserved for future read/write-grained policy; the current check is
// namespace-based and does not branch on it.
func authorize(p auth.Principal, namespace, project, action string) error {
	scope := strings.TrimSpace(p.Scope)
	if scope == "" || scope == "*" {
		return nil
	}
	if namespaceAllowed(scope, namespace, project) {
		return nil
	}
	return fmt.Errorf("token scope %q does not permit %s access to namespace %q", scope, action, namespace)
}

// namespaceAllowed reports whether any comma-separated grant in scope permits the
// (namespace, project) target. An empty namespace never matches a namespace grant.
func namespaceAllowed(scope, namespace, project string) bool {
	if namespace == "" {
		return false
	}
	for _, raw := range strings.Split(scope, ",") {
		grant := strings.TrimSpace(raw)
		switch {
		case grant == "*":
			return true
		case strings.HasPrefix(grant, "namespace:"):
			ns, proj, hasProj := strings.Cut(strings.TrimPrefix(grant, "namespace:"), "/")
			if ns != namespace {
				continue
			}
			if !hasProj || proj == "" {
				return true // namespace-wide grant
			}
			if proj == project {
				return true // project-scoped grant
			}
		}
	}
	return false
}
