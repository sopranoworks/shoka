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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
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
// When dumpHTTP is true (the server.debug.dump_http switch), it ALSO emits a verbatim
// dump of the full request and full response (method, path, all headers, full body,
// status) under the same id. B-59: the dump is RAW and UNREDACTED and is emitted as a
// guaranteed PAIR per request_id with no exception — every request and every response,
// every surface, every method/path, parseable or not, including SSE. dumpHTTP is false
// in every existing call path, so the default behaviour and existing logs are unchanged.
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

			// B-56/B-59: verbatim request dump, then capture the response body via the
			// recorder for the verbatim response dump. The request body is read fully and
			// RESTORED here — BEFORE any downstream reader (the MCP message path, the OAuth
			// form parser) — so the dump ALWAYS has the full request body AND the handler
			// still sees it unmodified (behaviour preserving). This is why a request can
			// never be missing its dump because its body was consumed downstream. B-59:
			// raw and unredacted — every header (incl. Authorization), the full URL with
			// query, and the full body verbatim. Default OFF; when ON it is complete.
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
					slog.String("url", r.URL.RequestURI()),
					slog.String("headers", headersVerbatim(r.Header)),
					slog.String("body", string(reqBody)),
				)
			}

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK, dump: dumpHTTP}
			next.ServeHTTP(sr, r)

			if dumpHTTP {
				// B-59: the response is ALWAYS dumped with its captured body — no exception,
				// SSE included. statusRecorder tees every Write into sr.body while forwarding
				// it to the client unchanged and preserving Flush, so the streamed bytes are
				// captured as they flush without altering the stream the client receives.
				logger.LogAttrs(ctx, slog.LevelInfo, "http response dump",
					slog.String("request_id", id),
					slog.String("surface", surface),
					slog.Int("status", sr.status),
					slog.String("headers", headersVerbatim(sr.Header())),
					slog.String("body", bodyForDump(sr.body)),
				)
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
	// B-56/B-59: when dump is set, Write tees the response body into body for the
	// verbatim dump. B-59 removed the SSE exception — the stream is teed like any other
	// response (the bytes are still forwarded immediately and Flush still passes
	// through, so the client's stream is unaffected), so no response is dumped bodyless.
	dump bool
	body *bytes.Buffer
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if w.dump {
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

// Hijack lets a terminal handler take over the connection — the WebSocket upgrade
// path on the Web listener (/ws/ui and /drafts/). It is the WebSocket analog of the
// Flush above: the recorder must stay transparent to the capabilities the underlying
// writer offers, or it silently breaks them. gorilla/websocket's Upgrade asserts
// http.Hijacker DIRECTLY on the writer it is handed (it does not walk Unwrap /
// http.ResponseController), so without this method every upgrade through this
// outermost middleware fails with "response does not implement http.Hijacker" (B-31).
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("reqtrace: underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
