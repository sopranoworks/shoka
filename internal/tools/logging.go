package tools

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
)

// LoggedTool wraps an MCP tool handler with structured logging and panic
// recovery. It logs metadata only (tool name, namespace/project/path, outcome,
// error class, duration) and NEVER the content argument or returned bytes.
// Logging is best-effort: a logging fault is swallowed and never alters the
// tool's result. A recovered panic is converted to an IsError result (matching
// Shoka's tool-error convention) so a panicking handler can never crash the
// server.
func LoggedTool[In, Out any](logger *slog.Logger, name string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		start := time.Now()
		safeLog(func() {
			logger.LogAttrs(ctx, slog.LevelInfo, "tool call received",
				append([]slog.Attr{slog.String("tool", name)}, metadataAttrs(in)...)...)
		})

		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				safeLog(func() {
					logger.LogAttrs(ctx, slog.LevelError, "tool handler panicked",
						slog.String("tool", name), slog.Any("panic", r), slog.String("stack", stack))
				})
				var zero Out
				res = &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "internal error"}},
				}
				out = zero
				err = nil
				safeLog(func() {
					logger.LogAttrs(ctx, slog.LevelError, "tool call completed",
						slog.String("tool", name), slog.String("outcome", "panic"),
						slog.Duration("duration", time.Since(start)))
				})
			}
		}()

		res, out, err = h(ctx, req, in)

		outcome, class := classify(res, err)
		safeLog(func() {
			// A non-nil Go error is an unexpected handler failure: surface it at
			// ERROR with the error (directive §3.3 "tool handler returned an
			// error"). Ordinary tool-level failures (res.IsError, nil Go error)
			// stay on the INFO "completed" line per the directive.
			if err != nil {
				logger.LogAttrs(ctx, slog.LevelError, "tool handler returned an error",
					slog.String("tool", name), slog.String("error_class", class), slog.Any("error", err))
			}
			attrs := []slog.Attr{
				slog.String("tool", name),
				slog.String("outcome", outcome),
				slog.Duration("duration", time.Since(start)),
			}
			if class != "" {
				attrs = append(attrs, slog.String("error_class", class))
			}
			logger.LogAttrs(ctx, slog.LevelInfo, "tool call completed", attrs...)
		})
		return res, out, err
	}
}

// safeLog runs fn, swallowing any panic from the logging path so a logging fault
// can never abort the request.
func safeLog(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// metadataAttrs extracts non-sensitive routing fields (namespace, project, path)
// from a tool input struct by name. It never reads content-bearing fields.
func metadataAttrs(in any) []slog.Attr {
	var attrs []slog.Attr
	v := reflect.ValueOf(in)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return attrs
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return attrs
	}
	for _, f := range []struct{ field, key string }{
		{"Namespace", "namespace"},
		{"ProjectName", "project"},
		{"Path", "path"},
	} {
		fv := v.FieldByName(f.field)
		if fv.IsValid() && fv.Kind() == reflect.String && fv.String() != "" {
			attrs = append(attrs, slog.String(f.key, fv.String()))
		}
	}
	return attrs
}

// classify maps a handler return into an (outcome, error_class) pair. Shoka
// handlers signal tool failures via res.IsError with a nil Go error, so the
// class is derived best-effort from the result text; a non-nil Go error is
// treated as version_conflict (typed) or internal.
func classify(res *mcp.CallToolResult, err error) (outcome, class string) {
	if err != nil {
		var vc *storage.VersionConflictError
		if errors.As(err, &vc) {
			return "error", "version_conflict"
		}
		return "error", "internal"
	}
	if res != nil && res.IsError {
		return "error", classifyText(firstText(res))
	}
	return "ok", ""
}

func classifyText(s string) string {
	ls := strings.ToLower(s)
	switch {
	case strings.Contains(ls, "version conflict"):
		return "version_conflict"
	case strings.Contains(ls, "not found"), strings.Contains(ls, "does not exist"), strings.Contains(ls, "no such file"):
		return "not_found"
	case strings.Contains(ls, "invalid"), strings.Contains(ls, "required"):
		return "validation_error"
	default:
		return "internal"
	}
}

func firstText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
