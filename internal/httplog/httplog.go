// Package httplog provides transport-layer logging for Shoka's MCP (SSE) endpoint.
// At INFO it logs SSE stream lifecycle and rejected requests (metadata only,
// never headers — so Authorization/?token= are never logged). At DEBUG it adds
// protocol-level observation: full JSON-RPC request/response bodies and SSE event
// payloads, with the directive's §4 redaction applied (see jsonrpc.go, sse.go).
// All DEBUG work is gated on logger.Enabled so INFO-level overhead is unchanged.
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

// Middleware logs SSE GET stream open/close (INFO), POST JSON-RPC messages
// (DEBUG, redacted), outbound SSE events (DEBUG, redacted), and any response with
// status >= 400 (WARN). A nil logger is replaced with a discard logger.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sessionID := r.URL.Query().Get("sessionid")
			debug := logger.Enabled(r.Context(), slog.LevelDebug)

			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			var rw http.ResponseWriter = sr

			switch r.Method {
			case http.MethodGet:
				connID := newConnID()
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream opened",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr))
				if debug {
					rw = &sseLogRecorder{statusRecorder: sr, logger: logger, ctx: r.Context(), connID: connID}
				}
				next.ServeHTTP(rw, r)
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream closed",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr),
					slog.Int("status", sr.status), slog.Duration("duration", time.Since(start)))
			case http.MethodPost:
				if debug {
					logRequest(r, logger, sessionID)
				}
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
func logRequest(r *http.Request, logger *slog.Logger, sessionID string) {
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
			slog.String("session_id", sessionID),
			slog.Int("body_bytes", len(body)),
			slog.String("remote", r.RemoteAddr))
		return
	}
	attrs := []slog.Attr{
		slog.String("rpc_method", method),
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
// for http.ResponseController) so SSE streaming is unaffected.
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
