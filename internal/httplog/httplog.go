// Package httplog provides transport-layer logging for Shoka's MCP endpoint,
// which is served over the Streamable HTTP transport. At INFO it logs the
// server->client stream lifecycle (GET), session termination (DELETE), and
// rejected requests (metadata only, never headers — so Authorization/?token= are
// never logged). At DEBUG it adds protocol-level observation: full JSON-RPC
// request/response bodies, with the directive's §4 redaction applied (see
// jsonrpc.go, stream.go). All DEBUG work is gated on logger.Enabled so INFO-level
// overhead is unchanged.
package httplog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// sessionIDHeader is the Streamable HTTP session identifier header (MCP spec,
// 2025-03-26 §Transports). The server assigns it on the initialize response and
// the client echoes it on every subsequent request. It is a correlation value,
// not a credential (the SDK exposes it on the wire), so logging it is safe — same
// status the SSE transport's ?sessionid= query value had.
const sessionIDHeader = "Mcp-Session-Id"

// Middleware observes the Streamable HTTP transport: the server->client stream
// open/close on GET (INFO), session termination on DELETE (INFO), POST JSON-RPC
// request bodies (DEBUG, redacted) and their responses (DEBUG, redacted, whether
// the SDK answers with application/json or a text/event-stream frame), and any
// response with status >= 400 (WARN). A nil logger is replaced with a discard
// logger.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			// On a Streamable HTTP request the session id rides the Mcp-Session-Id
			// header (the initialize POST has none — the server assigns it on the
			// response, captured below).
			sessionID := r.Header.Get(sessionIDHeader)
			debug := logger.Enabled(r.Context(), slog.LevelDebug)
			connID := newConnID()

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			var rw http.ResponseWriter = sr
			var rec *respRecorder
			if debug {
				rec = &respRecorder{statusRecorder: sr, logger: logger, ctx: r.Context(), connID: connID, sessionID: sessionID}
				rw = rec
			}

			switch r.Method {
			case http.MethodGet:
				// The standalone server->client SSE stream (optional per spec): a
				// long-lived hanging GET the SDK uses to push notifications.
				logger.LogAttrs(r.Context(), slog.LevelInfo, "mcp stream opened",
					slog.String("conn_id", connID), slog.String("session_id", sessionID),
					slog.String("remote", r.RemoteAddr))
				next.ServeHTTP(rw, r)
				if rec != nil {
					rec.finish()
				}
				logger.LogAttrs(r.Context(), slog.LevelInfo, "mcp stream closed",
					slog.String("conn_id", connID), slog.String("session_id", sessionID),
					slog.String("remote", r.RemoteAddr),
					slog.Int("status", sr.status), slog.Duration("duration", time.Since(start)))
			case http.MethodPost:
				if debug {
					logRequest(r, logger, sessionID, connID)
				}
				next.ServeHTTP(rw, r)
				if rec != nil {
					rec.finish()
				}
				// initialize assigns the session id on the response header; surface it
				// so the handshake's session is observable at DEBUG.
				if assigned := sr.Header().Get(sessionIDHeader); assigned != "" && assigned != sessionID {
					logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp session established",
						slog.String("conn_id", connID), slog.String("session_id", assigned),
						slog.String("remote", r.RemoteAddr))
				}
			case http.MethodDelete:
				logger.LogAttrs(r.Context(), slog.LevelInfo, "mcp session terminated",
					slog.String("conn_id", connID), slog.String("session_id", sessionID),
					slog.String("remote", r.RemoteAddr))
				next.ServeHTTP(rw, r)
			default:
				next.ServeHTTP(rw, r)
			}

			if sr.status >= 400 {
				logger.LogAttrs(r.Context(), slog.LevelWarn, "request rejected",
					slog.String("http_method", r.Method), slog.String("path", r.URL.Path),
					slog.String("session_id", sessionID), slog.Int("status", sr.status),
					slog.String("remote", r.RemoteAddr))
			}
		})
	}
}

// logRequest reads the full POST body, restores it byte-identically for the
// downstream handler, and logs the JSON-RPC method/id/params at DEBUG with §4
// redaction. Best-effort: a panic here never reaches the handler, and the body is
// always restored even on a read error. Only called when DEBUG is enabled, so the
// full-body read never happens at production INFO level.
func logRequest(r *http.Request, logger *slog.Logger, sessionID, connID string) {
	defer func() { _ = recover() }()
	if r.Body == nil {
		return
	}
	body, _ := io.ReadAll(r.Body)
	// Restore the exact bytes (plus any unread remainder) for the handler.
	r.Body = &restoredBody{Reader: io.MultiReader(bytes.NewReader(body), r.Body), closer: r.Body}

	method, id, params, ok := redactedRequest(body)
	if !ok {
		// Content-safe: never log raw bytes we could not structurally redact.
		logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp message received (unparseable)",
			slog.String("conn_id", connID),
			slog.String("session_id", sessionID),
			slog.Int("body_bytes", len(body)),
			slog.String("remote", r.RemoteAddr))
		return
	}
	attrs := []slog.Attr{
		slog.String("rpc_method", method),
		slog.String("conn_id", connID),
		slog.String("session_id", sessionID),
		slog.String("remote", r.RemoteAddr),
	}
	if id != "" {
		attrs = append(attrs, slog.String("rpc_id", id))
	}
	if params != "" {
		attrs = append(attrs, slog.String("rpc_params", params))
	}
	logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp message received", attrs...)
}

type restoredBody struct {
	io.Reader
	closer io.Closer
}

func (b *restoredBody) Close() error { return b.closer.Close() }

func newConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the response status while forwarding everything to the
// underlying ResponseWriter. It preserves http.Flusher (directly and via Unwrap,
// for http.ResponseController) so the transport's text/event-stream streaming is
// unaffected.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
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
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
