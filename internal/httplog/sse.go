// sse.go provides DEBUG-level observation of the outbound SSE stream. It wraps the
// http.ResponseWriter the SDK writes events to, forwards every byte UNCHANGED to
// the client, and logs a redacted COPY of each complete SSE event frame: the
// endpoint event (which carries ?sessionid=...) and message events (JSON-RPC
// responses, with §4 read-content redaction). It never alters wire bytes,
// ordering, or flushing; logging is best-effort and a logging panic can never
// reach the client.
package httplog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
)

// sseLogRecorder tees the SSE response stream for logging. It embeds
// *statusRecorder so status capture, Flush, and Unwrap (for
// http.ResponseController) are preserved unchanged.
type sseLogRecorder struct {
	*statusRecorder
	logger *slog.Logger
	ctx    context.Context
	connID string
	acc    []byte // accumulates bytes until a complete "\n\n"-delimited frame
}

// Write forwards the original bytes to the client first and unchanged, then
// observes a copy for logging.
func (w *sseLogRecorder) Write(b []byte) (int, error) {
	n, err := w.statusRecorder.Write(b)
	w.observe(b)
	return n, err
}

// observe appends written bytes and logs each complete SSE frame. Best-effort:
// any panic in the logging path is swallowed so it can never affect the response.
func (w *sseLogRecorder) observe(b []byte) {
	defer func() { _ = recover() }()
	w.acc = append(w.acc, b...)
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

func (w *sseLogRecorder) logFrame(frame []byte) {
	event, data := parseSSEFrame(frame)
	if event == "" && data == "" {
		return // keepalive comment / empty frame
	}
	if event == "message" {
		if id, redacted, ok := redactedResponse([]byte(data)); ok {
			w.logger.LogAttrs(w.ctx, slog.LevelDebug, "mcp response sent",
				slog.String("conn_id", w.connID),
				slog.String("event_name", event),
				slog.String("rpc_id", id),
				slog.String("event_data", redacted))
			return
		}
		// Unparseable message payload: log its size, never its raw bytes.
		w.logger.LogAttrs(w.ctx, slog.LevelDebug, "sse event sent",
			slog.String("conn_id", w.connID),
			slog.String("event_name", event),
			slog.Int("data_bytes", len(data)))
		return
	}
	// endpoint, ping, etc.: framing data carries no file content or credential.
	w.logger.LogAttrs(w.ctx, slog.LevelDebug, "sse event sent",
		slog.String("conn_id", w.connID),
		slog.String("event_name", event),
		slog.String("event_data", data))
}

// parseSSEFrame extracts the event name and concatenated data payload from one
// SSE frame (the bytes before a "\n\n" delimiter).
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
