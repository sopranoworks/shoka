package httplog

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B-52 §2.4: at INFO, the initialize handshake and the assigned session id are both
// visible — resolving the "session_id empty in every line" mystery (the id is read
// from the request header, empty until the server assigns one on the response,
// which was previously only surfaced at DEBUG).

func TestMiddleware_INFO_LogsInitializeAndAssignedSession(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "ASSIGNED-XYZ")
		w.WriteHeader(http.StatusOK)
	}))
	// initialize POST: no Mcp-Session-Id request header.
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize"}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "mcp initialize received") {
		t.Errorf("initialize handshake not logged at INFO: %q", out)
	}
	if !strings.Contains(out, "mcp session established") || !strings.Contains(out, "ASSIGNED-XYZ") {
		t.Errorf("assigned session id not logged at INFO: %q", out)
	}
}

// A session-bearing POST is NOT an initialize handshake: no "mcp initialize
// received" line (gated to the empty-session request).
func TestMiddleware_INFO_NoInitializeLineWhenSessionPresent(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/list"}`))
	req.Header.Set("Mcp-Session-Id", "SID-9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(buf.String(), "mcp initialize received") {
		t.Errorf("initialize line emitted for a session-bearing request: %q", buf.String())
	}
}
