package httplog

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dbgLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestMiddleware_GETLogsOpenClose(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "mcp stream opened") || !strings.Contains(out, "mcp stream closed") {
		t.Errorf("missing GET lifecycle: %q", out)
	}
}

func TestMiddleware_POSTLogsMethodAndSession_NotBody(t *testing.T) {
	lg, buf := dbgLogger()
	var seen string
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // downstream must still read the full body
		seen = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"content":"SECRET-BODY"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "ABC123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen != body {
		t.Fatalf("body not preserved for downstream:\n got: %q\nwant: %q", seen, body)
	}
	out := buf.String()
	if !strings.Contains(out, `rpc_method=tools/call`) {
		t.Errorf("method not logged: %q", out)
	}
	if !strings.Contains(out, "ABC123") {
		t.Errorf("session id not logged: %q", out)
	}
	if strings.Contains(out, "SECRET-BODY") {
		t.Errorf("request body leaked into logs: %q", out)
	}
}

func TestMiddleware_LogsAssignedSessionOnInitialize(t *testing.T) {
	lg, buf := dbgLogger()
	// The initialize POST carries no session id; the server assigns it on the
	// response header (Streamable HTTP), which the middleware surfaces.
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "ASSIGNED-XYZ")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"initialize"}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "mcp session established") || !strings.Contains(out, "ASSIGNED-XYZ") {
		t.Errorf("assigned session id not logged: %q", out)
	}
}

func TestMiddleware_LogsDeleteAsTermination(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "DEL-SESSION")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "mcp session terminated") || !strings.Contains(out, "DEL-SESSION") {
		t.Errorf("DELETE termination not logged: %q", out)
	}
}

func TestMiddleware_WarnsOnError(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/call"}`))
	req.Header.Set("Mcp-Session-Id", "stale-session")
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "request rejected") || !strings.Contains(out, "status=404") {
		t.Errorf("expected WARN with status 404: %q", out)
	}
	if !strings.Contains(out, "stale-session") {
		t.Errorf("stale session id should appear on the rejection line: %q", out)
	}
}

func TestMiddleware_NeverLogsAuthHeader(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/list"}`))
	req.Header.Set("Mcp-Session-Id", "ABC")
	req.Header.Set("Authorization", "Bearer SUPER-SECRET-TOKEN")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(buf.String(), "SUPER-SECRET-TOKEN") {
		t.Errorf("auth token leaked into logs: %q", buf.String())
	}
}

func TestMiddleware_PreservesFlusher(t *testing.T) {
	lg, _ := dbgLogger()
	flushed := false
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if !flushed {
		t.Error("wrapped ResponseWriter lost http.Flusher; event-stream streaming would break")
	}
}

// At INFO level the protocol-logging work must not run: the body must reach the
// handler intact and no DEBUG "mcp message received" line may be emitted. This
// locks in the directive's performance contract (DEBUG-gated full-body reads).
func TestMiddleware_POST_InfoLevel_BodyNotReadAndNoDebugLine(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var seen string
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "X")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != body {
		t.Fatalf("INFO-level middleware corrupted POST body: got %q", seen)
	}
	if strings.Contains(buf.String(), "mcp message received") {
		t.Errorf("DEBUG line emitted at INFO level: %q", buf.String())
	}
}

// A nil logger must be tolerated (best-effort logging must never fail a request).
func TestMiddleware_NilLoggerDoesNotPanic(t *testing.T) {
	served := false
	h := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/list"}`)))
	if !served {
		t.Error("downstream handler was not invoked with a nil logger")
	}
}
