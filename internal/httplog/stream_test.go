package httplog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestRecorder builds a respRecorder over an httptest recorder, logging at
// DEBUG into sink. contentType seeds the response header so the recorder picks
// its observation mode (event-stream vs json) exactly as it would in production.
func newTestRecorder(under http.ResponseWriter, sink io.Writer, contentType string) *respRecorder {
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	under.Header().Set("Content-Type", contentType)
	sr := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	return &respRecorder{statusRecorder: sr, logger: logger, ctx: context.Background(), connID: "c1", sessionID: "s1"}
}

func messageFrame(t *testing.T, id int, body string) []byte {
	t.Helper()
	mirror, _ := json.Marshal(map[string]any{"content": body, "version": "vhash"})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body, "version": "vhash"},
		},
	})
	frame := append([]byte("event: message\nid: 42_1\ndata: "), resp...)
	return append(frame, '\n', '\n')
}

func TestRespRecorder_ForwardsBytesUnchanged(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestRecorder(under, io.Discard, "text/event-stream")
	frame := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
	n, err := w.Write(frame)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(frame) {
		t.Errorf("n = %d, want %d", n, len(frame))
	}
	if under.Body.String() != string(frame) {
		t.Errorf("client bytes altered: %q", under.Body.String())
	}
}

func TestRespRecorder_EventStreamMessageRedacted(t *testing.T) {
	const body = "SECRET-BODY-on-the-wire-987"
	frame := messageFrame(t, 4, body)

	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestRecorder(under, &sink, "text/event-stream")
	w.Write(frame)

	if under.Body.String() != string(frame) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, body) {
		t.Fatalf("file body leaked into logs: %s", logs)
	}
	for _, want := range []string{"mcp response sent", "rpc_id=4", "vhash", "request_id=c1", "session_id=s1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

// The SDK answers a POST with application/json when JSONResponse mode is on; the
// recorder buffers the body and logs it from finish().
func TestRespRecorder_JSONResponseRedacted(t *testing.T) {
	const body = "SECRET-JSON-BODY-555"
	mirror, _ := json.Marshal(map[string]any{"content": body, "version": "vh2"})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body, "version": "vh2"},
		},
	})

	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestRecorder(under, &sink, "application/json")
	w.Write(resp)
	w.finish()

	if under.Body.String() != string(resp) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, body) {
		t.Fatalf("file body leaked into logs: %s", logs)
	}
	for _, want := range []string{"mcp response sent", "rpc_id=7", "vh2"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestRespRecorder_MultipleFramesInOneWrite(t *testing.T) {
	var sink bytes.Buffer
	w := newTestRecorder(httptest.NewRecorder(), &sink, "text/event-stream")
	w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n"))
	logs := sink.String()
	if !strings.Contains(logs, "rpc_id=1") || !strings.Contains(logs, "rpc_id=2") {
		t.Errorf("expected both responses logged: %s", logs)
	}
}

func TestRespRecorder_FrameSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newTestRecorder(httptest.NewRecorder(), &sink, "text/event-stream")
	w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":9,\"resu"))
	if strings.Contains(sink.String(), "mcp response sent") {
		t.Fatal("must not log a partial frame")
	}
	w.Write([]byte("lt\":{}}\n\n"))
	if !strings.Contains(sink.String(), "rpc_id=9") {
		t.Errorf("frame completed across writes should log: %s", sink.String())
	}
}

func TestRespRecorder_PreservesFlusherViaResponseController(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestRecorder(under, io.Discard, "text/event-stream")
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("Flush via ResponseController failed: %v", err)
	}
}

// A server->client notification frame (no result/error) is not a response; its
// payload must never be logged raw, only its event name and size.
func TestRespRecorder_NonResponseFrameNotLeaked(t *testing.T) {
	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestRecorder(under, &sink, "text/event-stream")
	frame := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/secret\",\"params\":{\"x\":\"DO-NOT-LOG-123\"}}\n\n")
	w.Write(frame)
	if under.Body.String() != string(frame) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, "DO-NOT-LOG-123") {
		t.Fatalf("notification payload must not be logged raw: %s", logs)
	}
	if !strings.Contains(logs, "mcp event sent") || !strings.Contains(logs, "data_bytes=") {
		t.Errorf("expected event-name + data_bytes fallback log: %s", logs)
	}
}

func TestParseSSEFrame(t *testing.T) {
	ev, data := parseSSEFrame([]byte("event: message\nid: 1_2\ndata: {\"a\":1}"))
	if ev != "message" || data != `{"a":1}` {
		t.Errorf("got (%q,%q)", ev, data)
	}
	ev, data = parseSSEFrame([]byte("data: line1\ndata: line2"))
	if ev != "" || data != "line1\nline2" {
		t.Errorf("multi-line data: got (%q,%q)", ev, data)
	}
}
