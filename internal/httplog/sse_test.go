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

// newTestSSERecorder builds an sseLogRecorder over an httptest recorder, logging
// at DEBUG into sink.
func newTestSSERecorder(under http.ResponseWriter, sink io.Writer) *sseLogRecorder {
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sr := &statusRecorder{ResponseWriter: under, status: http.StatusOK}
	return &sseLogRecorder{statusRecorder: sr, logger: logger, ctx: context.Background(), connID: "c1"}
}

func TestSSELogRecorder_ForwardsBytesUnchanged(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, io.Discard)
	frame := []byte("event: endpoint\ndata: ?sessionid=ABC\n\n")
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

func TestSSELogRecorder_EndpointEventLoggedVerbatim(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sessionid=SESS123\n\n"))
	logs := sink.String()
	for _, want := range []string{"sse event sent", "endpoint", "SESS123", "conn_id=c1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestSSELogRecorder_MessageReadResultRedacted(t *testing.T) {
	const body = "SECRET-BODY-on-the-wire-987"
	mirror, _ := json.Marshal(map[string]any{"content": body, "version": "vhash"})
	resp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 4,
		"result": map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": string(mirror)}},
			"structuredContent": map[string]any{"content": body, "version": "vhash"},
		},
	})
	frame := append([]byte("event: message\ndata: "), resp...)
	frame = append(frame, '\n', '\n')

	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, &sink)
	w.Write(frame)

	if under.Body.String() != string(frame) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, body) {
		t.Fatalf("file body leaked into logs: %s", logs)
	}
	for _, want := range []string{"mcp response sent", "rpc_id=4", "vhash"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q: %s", want, logs)
		}
	}
}

func TestSSELogRecorder_MultipleFramesInOneWrite(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sessionid=A\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	logs := sink.String()
	if strings.Count(logs, "event_name=endpoint") != 1 {
		t.Errorf("expected one endpoint log: %s", logs)
	}
	if !strings.Contains(logs, "rpc_id=1") {
		t.Errorf("expected message response logged: %s", logs)
	}
}

func TestSSELogRecorder_FrameSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newTestSSERecorder(httptest.NewRecorder(), &sink)
	w.Write([]byte("event: endpoint\ndata: ?sess"))
	if strings.Contains(sink.String(), "sse event sent") {
		t.Fatal("must not log a partial frame")
	}
	w.Write([]byte("ionid=Z\n\n"))
	if !strings.Contains(sink.String(), "sessionid=Z") {
		t.Errorf("frame completed across writes should log: %s", sink.String())
	}
}

func TestSSELogRecorder_PreservesFlusherViaResponseController(t *testing.T) {
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, io.Discard)
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("Flush via ResponseController failed: %v", err)
	}
}

func TestSSELogRecorder_UnparseableMessageNotLeaked(t *testing.T) {
	var sink bytes.Buffer
	under := httptest.NewRecorder()
	w := newTestSSERecorder(under, &sink)
	frame := []byte("event: message\ndata: not-json-at-all\n\n")
	w.Write(frame)
	if under.Body.String() != string(frame) {
		t.Fatal("client bytes must be unchanged")
	}
	logs := sink.String()
	if strings.Contains(logs, "not-json-at-all") {
		t.Fatalf("unparseable message payload must not be logged raw: %s", logs)
	}
	if !strings.Contains(logs, "data_bytes=") {
		t.Errorf("expected data_bytes fallback log: %s", logs)
	}
}

func TestParseSSEFrame(t *testing.T) {
	ev, data := parseSSEFrame([]byte("event: message\ndata: {\"a\":1}"))
	if ev != "message" || data != `{"a":1}` {
		t.Errorf("got (%q,%q)", ev, data)
	}
	ev, data = parseSSEFrame([]byte("data: line1\ndata: line2"))
	if ev != "" || data != "line1\nline2" {
		t.Errorf("multi-line data: got (%q,%q)", ev, data)
	}
}
