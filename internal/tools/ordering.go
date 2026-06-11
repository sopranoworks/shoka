package tools

import (
	"context"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// coreToolOrder is the operator-fixed core-first sequence for the tools/list
// response (B-49 fix-1). These eight read/write tools are the ones every Shoka
// session depends on; they must appear first, in exactly this order. All other
// registered tools follow, keeping their existing (alphabetical) relative order.
var coreToolOrder = []string{
	"read_file",
	"write_file",
	"list_files",
	"read_summary",
	"search_files",
	"get_history",
	"patch_file",
	"append_to_file",
}

// CoreFirstToolsMiddleware returns an MCP receiving middleware that reorders the
// tools/list response so the eight core tools (coreToolOrder) come first, in that
// order, ahead of all other tools. It is registered with
// Server.AddReceivingMiddleware: the middleware calls the inner handler, then
// reorders the resulting *ListToolsResult.Tools before it is serialized.
//
// The mcp-go-sdk (v1.6.0) stores tools in a name-keyed featureSet and emits
// tools/list sorted alphabetically by name (features.go sortKeys), with no
// server-side ordering hook. This middleware takes over only the listing order;
// it does not touch tool registration or tools/call dispatch — it no-ops for
// every method other than tools/list (B-49 fix-1, route §2.2).
//
// Single-page reliance: Shoka registers ~18 tools and leaves PageSize at the SDK
// default (1000), so the entire catalog returns in one page (NextCursor empty)
// and reordering that page is globally core-first. If pagination below the tool
// count were ever enabled, core-first would have to be applied at the listing
// source rather than post-pagination, so as not to disturb cursor continuity;
// the guard below leaves a multi-page response untouched rather than corrupt it.
func CoreFirstToolsMiddleware() mcp.Middleware {
	rank := make(map[string]int, len(coreToolOrder))
	for i, name := range coreToolOrder {
		rank[name] = i
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return res, err
			}
			lt, ok := res.(*mcp.ListToolsResult)
			if !ok || lt == nil {
				return res, err
			}
			// Only reorder a complete, single-page response. A paginated response
			// (non-empty cursor) is left as-is to preserve cursor continuity.
			if lt.NextCursor != "" {
				return res, err
			}
			reorderCoreFirst(lt.Tools, rank)
			return res, err
		}
	}
}

// reorderCoreFirst stably reorders tools so that those named in rank come first,
// ordered by their rank value; all other tools keep their existing relative
// order after the core block. No tool is added or removed.
func reorderCoreFirst(toolsList []*mcp.Tool, rank map[string]int) {
	sort.SliceStable(toolsList, func(i, j int) bool {
		ri, iCore := rank[toolsList[i].Name]
		rj, jCore := rank[toolsList[j].Name]
		switch {
		case iCore && jCore:
			return ri < rj
		case iCore:
			return true
		case jCore:
			return false
		default:
			return false // both non-core: preserve existing (stable) order
		}
	})
}
