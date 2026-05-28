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
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "sse stream opened") || !strings.Contains(out, "sse stream closed") {
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
	req := httptest.NewRequest(http.MethodPost, "/sse?sessionid=ABC123", strings.NewReader(body))
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

func TestMiddleware_WarnsOnError(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	req := httptest.NewRequest(http.MethodPost, "/sse?sessionid=missing", strings.NewReader(`{"method":"tools/call"}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "request rejected") || !strings.Contains(out, "status=404") {
		t.Errorf("expected WARN with status 404: %q", out)
	}
}

func TestMiddleware_NeverLogsAuthHeader(t *testing.T) {
	lg, buf := dbgLogger()
	h := Middleware(lg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/sse?sessionid=ABC", strings.NewReader(`{"method":"tools/list"}`))
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
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/sse", nil))
	if !flushed {
		t.Error("wrapped ResponseWriter lost http.Flusher; SSE streaming would break")
	}
}

// A nil logger must be tolerated (best-effort logging must never fail a request).
func TestMiddleware_NilLoggerDoesNotPanic(t *testing.T) {
	served := false
	h := Middleware(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/sse?sessionid=x", strings.NewReader(`{"method":"tools/list"}`)))
	if !served {
		t.Error("downstream handler was not invoked with a nil logger")
	}
}
