// stream.go provides DEBUG-level observation of the Streamable HTTP transport's
// responses. It wraps the http.ResponseWriter the SDK writes to, forwards every
// byte UNCHANGED to the client, and logs a redacted COPY of each JSON-RPC
// response. The SDK answers a POST with either a single application/json body or
// a text/event-stream of frames (the spec's two response modes), and pushes
// server->client messages on the standalone GET stream as text/event-stream
// frames; this recorder handles both. It never alters wire bytes, ordering, or
// flushing; logging is best-effort and a logging panic can never reach the
// client.
package httplog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
)

// response observation modes, decided lazily from the response Content-Type.
const (
	respModeUnknown = iota
	respModeEventStream
	respModeJSON
)

// respRecorder tees the response stream for logging. It embeds *statusRecorder so
// status capture, Flush, and Unwrap (for http.ResponseController) are preserved
// unchanged.
type respRecorder struct {
	*statusRecorder
	logger    *slog.Logger
	ctx       context.Context
	connID    string
	sessionID string
	mode      int
	acc       []byte // event-stream: bytes until "\n\n"; json: the whole body
}

// Write forwards the original bytes to the client first and unchanged, then
// observes a copy for logging.
func (w *respRecorder) Write(b []byte) (int, error) {
	n, err := w.statusRecorder.Write(b)
	w.observe(b)
	return n, err
}

// observe accumulates written bytes. For a text/event-stream response it logs
// each complete "\n\n"-delimited frame as it arrives (so streaming is observed
// live); for an application/json response the body is buffered and logged once by
// finish(). Best-effort: any panic in the logging path is swallowed.
func (w *respRecorder) observe(b []byte) {
	defer func() { _ = recover() }()
	if w.mode == respModeUnknown {
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			w.mode = respModeEventStream
		} else {
			w.mode = respModeJSON
		}
	}
	w.acc = append(w.acc, b...)
	if w.mode != respModeEventStream {
		return
	}
	for {
		idx := bytes.Index(w.acc, []byte("\n\n"))
		if idx < 0 {
			break
		}
		w.logFrame(w.acc[:idx])
		w.acc = w.acc[idx+2:]
	}
	if len(w.acc) == 0 {
		w.acc = nil
	}
}

// finish logs a buffered application/json response body once the handler has
// returned. It is a no-op for event-stream responses (already logged per frame)
// and best-effort.
func (w *respRecorder) finish() {
	defer func() { _ = recover() }()
	if w.mode != respModeJSON || len(w.acc) == 0 {
		return
	}
	w.logMessage(w.acc)
}

// logFrame logs one SSE frame (the bytes before a "\n\n" delimiter) from a
// text/event-stream response.
func (w *respRecorder) logFrame(frame []byte) {
	event, data := parseSSEFrame(frame)
	if event == "" && data == "" {
		return // keepalive comment / empty frame
	}
	if w.logMessage([]byte(data)) {
		return
	}
	// Not a JSON-RPC response (a server->client notification/request, ping, etc.).
	// Log only its event name and size — never raw data, which could in principle
	// carry payload we have not structurally redacted.
	w.logger.LogAttrs(w.ctx, slog.LevelDebug, "mcp event sent",
		slog.String("conn_id", w.connID),
		slog.String("session_id", w.sessionID),
		slog.String("event_name", event),
		slog.Int("data_bytes", len(data)))
}

// logMessage logs a JSON-RPC response payload with §4 read-content redaction. It
// returns false (logging nothing) if data is not a parseable JSON-RPC response,
// so callers can fall back to a content-safe alternative.
func (w *respRecorder) logMessage(data []byte) bool {
	id, redacted, ok := redactedResponse(data)
	if !ok {
		return false
	}
	w.logger.LogAttrs(w.ctx, slog.LevelDebug, "mcp response sent",
		slog.String("conn_id", w.connID),
		slog.String("session_id", w.sessionID),
		slog.String("rpc_id", id),
		slog.String("event_data", redacted))
	return true
}

// parseSSEFrame extracts the event name and concatenated data payload from one
// SSE frame (the bytes before a "\n\n" delimiter). Non event:/data: lines (id:,
// retry:, comments) are ignored.
func parseSSEFrame(frame []byte) (event, data string) {
	for _, line := range strings.Split(string(frame), "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			// The SDK emits "data: <payload>" with exactly one space; trim only
			// that (not TrimSpace, which would strip meaningful payload whitespace).
			d := strings.TrimPrefix(line[len("data:"):], " ")
			if data == "" {
				data = d
			} else {
				data += "\n" + d
			}
		}
	}
	return event, data
}
