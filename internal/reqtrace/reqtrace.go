// Package reqtrace makes end-to-end request traceability a structural property of
// every Shoka listener (B-53). One outermost middleware is applied to each of the
// three listeners (Web, MCP-plain, MCP-oauth); it assigns a per-request correlation
// id, records the raw inbound request at entry, and records the response (status +
// reason category + routing) at exit — all sharing the one id. Inner layers
// (httplog, auth, oauth, discovery) read the id from the request context with ID()
// and fold their existing lines onto it, so a single failed request is diagnosable
// from one correlated trace, on any listener, with no path exempt.
//
// Confidentiality: the entry record carries header PRESENCE bools only, never the
// Authorization or Mcp-Session-Id VALUES, and logs r.URL.Path (never the raw query)
// so a WebSocket ?token= never appears. The surface is a fixed category label
// ("web"/"mcp-plain"/"mcp-oauth"), never a listen address.
package reqtrace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// sessionIDHeader is the Streamable HTTP session identifier header. Only its
// PRESENCE is recorded at entry (the value is logged elsewhere, where it is a
// known-safe correlation value — here we keep the raw-inbound record to presence
// bools per the directive §2.3).
const sessionIDHeader = "Mcp-Session-Id"

type ctxKey int

const (
	idKey ctxKey = iota
	routeKey
)

// ID returns the per-request correlation id carried on ctx, or "" if the request
// did not pass through Middleware (e.g. a unit test exercising an inner layer
// directly). Inner layers use this to stamp their lines with the shared id.
func ID(ctx context.Context) string {
	id, _ := ctx.Value(idKey).(string)
	return id
}

// routeBox is a mutable cell so a terminal handler (deep in the chain) can record
// which route it dispatched to and have the outermost Middleware read it back when
// it writes the response-stage line.
type routeBox struct{ name string }

// SetRoute records the routing stage — which terminal handler the request reached
// (e.g. "mcp-dispatch", "oauth-token", "web-api"). It is read into the
// response-stage line. A no-op when ctx carries no trace cell, so inner handlers
// stay safe to call in isolation.
func SetRoute(ctx context.Context, name string) {
	if b, ok := ctx.Value(routeKey).(*routeBox); ok {
		b.name = name
	}
}

// Route wraps next so that entering it tags the request's routing stage with name.
// Apply it at each route registration so every dispatched handler is identified
// structurally (rather than logging the route at hand-picked sites).
func Route(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetRoute(r.Context(), name)
		next.ServeHTTP(w, r)
	})
}

// Middleware returns the outermost tracing middleware for the listener identified
// by surface ("web" / "mcp-plain" / "mcp-oauth"). It assigns a per-request id into
// the request context, logs the raw-inbound entry record (§2.3), and logs the
// response record with status + reason category + route (§2.5) — all under the one
// id. A nil logger is replaced with a discard logger.
//
// When dumpHTTP is true (the B-56 server.debug.dump_http switch), it ALSO emits a
// verbatim dump of the full request and full response (method, path, all headers,
// full body, status) under the same id — secrets redacted to a fixed marker, nothing
// else processed. dumpHTTP is false in every existing call path, so the default
// behaviour and existing logs are unchanged.
func Middleware(logger *slog.Logger, surface string, dumpHTTP bool) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := newID()
			// Captured for the response-stage line: the Mcp-Session-Id is a non-secret
			// correlation value (the SDK exposes it on the wire; logged elsewhere per
			// B-52), so on a rejected MCP request the response line can name the stale
			// session — recovering what httplog's removed "request rejected" line carried.
			sessionID := r.Header.Get(sessionIDHeader)
			box := &routeBox{}
			ctx := context.WithValue(r.Context(), idKey, id)
			ctx = context.WithValue(ctx, routeKey, box)
			r = r.WithContext(ctx)

			// §2.3 raw inbound: surface/port (as a category), method, path AS RECEIVED
			// (exposes proxy rewrite such as /mcp→/), header-presence bools (never the
			// values), content-type. r.URL.Path excludes the query, so a WS ?token= is
			// never logged.
			logger.LogAttrs(ctx, slog.LevelInfo, "request received",
				slog.String("request_id", id),
				slog.String("surface", surface),
				slog.String("http_method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Bool("authorization_present", r.Header.Get("Authorization") != ""),
				slog.Bool("mcp_session_id_present", r.Header.Get(sessionIDHeader) != ""),
				slog.String("content_type", r.Header.Get("Content-Type")),
			)

			// B-56: verbatim request dump (secrets redacted), then capture the response
			// body via the recorder for the verbatim response dump. The request body is
			// read fully and RESTORED so the handler sees it unmodified — behaviour
			// preserving. Default OFF; when ON it is complete (no field selection).
			if dumpHTTP {
				var reqBody []byte
				if r.Body != nil {
					reqBody, _ = io.ReadAll(r.Body)
					_ = r.Body.Close()
					r.Body = io.NopCloser(bytes.NewReader(reqBody))
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "http request dump",
					slog.String("request_id", id),
					slog.String("surface", surface),
					slog.String("http_method", r.Method),
					slog.String("url", redactURL(r.URL)),
					slog.String("headers", redactHeaders(r.Header)),
					slog.String("body", string(redactBody(reqBody))),
				)
			}

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK, dump: dumpHTTP}
			next.ServeHTTP(sr, r)

			if dumpHTTP {
				attrs := []slog.Attr{
					slog.String("request_id", id),
					slog.String("surface", surface),
					slog.Int("status", sr.status),
					slog.String("headers", redactHeaders(sr.Header())),
				}
				if sr.streaming {
					// The MCP SSE stream is long-lived; buffering its body would alter flush
					// timing and grow unbounded — the documented capture limit. Headers +
					// status are still dumped.
					attrs = append(attrs, slog.String("body", ""), slog.String("body_omitted", "streaming"))
				} else {
					attrs = append(attrs, slog.String("body", bodyForDump(sr.body)))
				}
				logger.LogAttrs(ctx, slog.LevelInfo, "http response dump", attrs...)
			}

			// §2.5 response: status + (on non-2xx) a reason category + the routing
			// stage, under the same id. The route is "unrouted" when the request never
			// reached a tagged terminal handler (e.g. a pre-routing auth 401) — which is
			// exactly how the path=/ failing case shows it was blocked before routing.
			route := box.name
			if route == "" {
				route = "unrouted"
			}
			level := slog.LevelInfo
			attrs := []slog.Attr{
				slog.String("request_id", id),
				slog.String("surface", surface),
				slog.String("http_method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", sr.status),
				slog.Duration("duration", time.Since(start)),
			}
			if sessionID != "" {
				attrs = append(attrs, slog.String("session_id", sessionID))
			}
			if sr.status >= 400 {
				level = slog.LevelWarn
				attrs = append(attrs, slog.String("reason", reasonForStatus(sr.status)))
			}
			logger.LogAttrs(ctx, level, "request completed", attrs...)
		})
	}
}

// reasonForStatus maps a non-2xx status to a coarse reason category for the
// response line. The PRECISE reason (e.g. which discrete auth failure) is on the
// correlated auth-stage line; this is the response-stage summary under the same id.
func reasonForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad-request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not-found"
	case http.StatusMethodNotAllowed:
		return "method-not-allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusServiceUnavailable:
		return "service-unavailable"
	}
	switch {
	case status >= 500:
		return "server-error"
	case status >= 400:
		return "client-error"
	default:
		return "non-2xx"
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the response status while forwarding everything to the
// underlying ResponseWriter. It preserves http.Flusher (directly and via Unwrap,
// for http.ResponseController) so the Streamable HTTP transport's text/event-stream
// streaming is unaffected — identical to httplog's recorder, since this middleware
// nests outside httplog on the MCP listeners.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	// B-56: when dump is set, Write tees the response body into body for the verbatim
	// dump — EXCEPT once the response is detected as an SSE stream (streaming), where
	// teeing the unbounded long-lived body is skipped to preserve flush behaviour.
	dump      bool
	streaming bool
	body      *bytes.Buffer
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
		if w.dump && isEventStream(w.Header()) {
			w.streaming = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
		if w.dump && isEventStream(w.Header()) {
			w.streaming = true
		}
	}
	if w.dump && !w.streaming {
		if w.body == nil {
			w.body = &bytes.Buffer{}
		}
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
