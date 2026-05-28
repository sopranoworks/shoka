// Package httplog provides transport-layer logging middleware for Shoka's MCP
// (SSE) endpoint. It logs request metadata only — never request headers (so
// Authorization/?token= are never logged) and never the message params/content
// (only the top-level JSON-RPC "method").
package httplog

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

// methodRe extracts the top-level JSON-RPC method name from the start of a
// message body. Matched against a bounded prefix only.
var methodRe = regexp.MustCompile(`"method"\s*:\s*"([^"]+)"`)

// Middleware logs SSE GET stream open/close (INFO), POST message receipt with
// JSON-RPC method + session id (DEBUG), and any response with status >= 400
// (WARN). A nil logger is replaced with a discard logger.
func Middleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sessionID := r.URL.Query().Get("sessionid")
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			switch r.Method {
			case http.MethodGet:
				connID := newConnID()
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream opened",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr))
				next.ServeHTTP(rec, r)
				logger.LogAttrs(r.Context(), slog.LevelInfo, "sse stream closed",
					slog.String("conn_id", connID), slog.String("remote", r.RemoteAddr),
					slog.Int("status", rec.status), slog.Duration("duration", time.Since(start)))
			case http.MethodPost:
				method := peekMethod(r)
				logger.LogAttrs(r.Context(), slog.LevelDebug, "mcp message received",
					slog.String("rpc_method", method), slog.String("session_id", sessionID),
					slog.String("remote", r.RemoteAddr))
				next.ServeHTTP(rec, r)
			default:
				next.ServeHTTP(rec, r)
			}

			if rec.status >= 400 {
				logger.LogAttrs(r.Context(), slog.LevelWarn, "request rejected",
					slog.String("http_method", r.Method), slog.String("path", r.URL.Path),
					slog.String("session_id", sessionID), slog.Int("status", rec.status),
					slog.String("remote", r.RemoteAddr))
			}
		})
	}
}

// peekMethod reads only a bounded prefix of the body to extract the JSON-RPC
// method, then restores the full body (prefix + untouched remainder) so the
// downstream handler reads the complete, unmodified stream. The large content
// body is never buffered.
func peekMethod(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	const peekMax = 1024
	prefix, err := io.ReadAll(io.LimitReader(r.Body, peekMax))
	// Always restore a working body, even on a partial read.
	r.Body = &restoredBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), r.Body),
		closer: r.Body,
	}
	if err != nil {
		return ""
	}
	if m := methodRe.FindSubmatch(prefix); m != nil {
		return string(m[1])
	}
	return ""
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
// underlying ResponseWriter. It preserves http.Flusher (directly and via
// Unwrap, for http.ResponseController) so SSE streaming is unaffected.
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
