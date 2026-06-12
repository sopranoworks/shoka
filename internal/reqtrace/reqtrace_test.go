package reqtrace

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonLogger returns a slog logger writing JSON to a buffer, plus the buffer.
func jsonLogger() (*slog.Logger, *strings.Builder) {
	var b strings.Builder
	l := slog.New(slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return l, &b
}

func TestMiddleware_EntryAndResponse_ShareOneID(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", false)(Route("mcp-dispatch", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "request received") || !strings.Contains(out, "request completed") {
		t.Fatalf("expected both entry and response lines, got:\n%s", out)
	}
	if !strings.Contains(out, `"surface":"mcp-oauth"`) {
		t.Errorf("expected surface label, got:\n%s", out)
	}
	if !strings.Contains(out, `"route":"mcp-dispatch"`) {
		t.Errorf("expected routing stage on response, got:\n%s", out)
	}
	// Both lines must carry the same request_id (correlation).
	ids := extractIDs(out)
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Errorf("expected one shared request_id across both lines, got %v", ids)
	}
}

func TestMiddleware_RejectCarriesReasonAndID(t *testing.T) {
	logger, buf := jsonLogger()
	// Handler that 401s WITHOUT being tagged → response must show route=unrouted +
	// reason=unauthorized (the live pre-routing 401 shape).
	h := Middleware(logger, "mcp-oauth", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))

	out := buf.String()
	if !strings.Contains(out, `"status":401`) || !strings.Contains(out, `"reason":"unauthorized"`) {
		t.Errorf("expected status+reason on reject, got:\n%s", out)
	}
	if !strings.Contains(out, `"route":"unrouted"`) {
		t.Errorf("expected route=unrouted for a pre-routing 401, got:\n%s", out)
	}
}

func TestMiddleware_NeverLogsAuthHeaderOrToken(t *testing.T) {
	logger, buf := jsonLogger()
	h := Middleware(logger, "web", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ws/ui?token=SUPERSECRET", nil)
	req.Header.Set("Authorization", "Bearer SUPERSECRET")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "SUPERSECRET") {
		t.Fatalf("secret leaked into trace (Authorization value or ?token=): %s", out)
	}
	if !strings.Contains(out, `"authorization_present":true`) {
		t.Errorf("expected authorization_present bool, got:\n%s", out)
	}
	// Path must be logged without the query string.
	if !strings.Contains(out, `"path":"/ws/ui"`) {
		t.Errorf("expected bare path without query, got:\n%s", out)
	}
}

func TestMiddleware_StaleSessionRejectNamesSession(t *testing.T) {
	// The restart-recovery case: a POST echoing a stale Mcp-Session-Id that the
	// server 404s. The response line must name the stale session (recovering what
	// httplog's removed reject line carried) under the shared id.
	logger, buf := jsonLogger()
	h := Middleware(logger, "mcp-oauth", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "stale-session")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, `"status":404`) || !strings.Contains(out, `"reason":"not-found"`) {
		t.Errorf("expected 404 + not-found reason, got:\n%s", out)
	}
	if !strings.Contains(out, `"session_id":"stale-session"`) {
		t.Errorf("expected the stale session id on the response line, got:\n%s", out)
	}
}

func TestID_RoundTripsThroughContext(t *testing.T) {
	var seen string
	h := Middleware(slog.New(slog.DiscardHandler), "mcp-plain", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = ID(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if seen == "" {
		t.Fatal("expected a non-empty request id in the handler context")
	}
}

func TestID_EmptyWithoutMiddleware(t *testing.T) {
	if ID(context.Background()) != "" {
		t.Error("expected empty id when the request never passed through Middleware")
	}
}

func TestSetRoute_NoopWithoutCell(t *testing.T) {
	// Must not panic when called on a bare context (inner handler in isolation).
	SetRoute(context.Background(), "x")
}

func TestMiddleware_PreservesFlusher(t *testing.T) {
	flushed := false
	h := Middleware(slog.New(slog.DiscardHandler), "mcp-oauth", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flushed = true
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !flushed {
		t.Error("expected the wrapped ResponseWriter to expose http.Flusher for SSE streaming")
	}
}

func TestMiddleware_NilLoggerDoesNotPanic(t *testing.T) {
	h := Middleware(nil, "web", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// extractIDs pulls every "request_id":"..." value from JSON log output, in order.
func extractIDs(out string) []string {
	var ids []string
	const key = `"request_id":"`
	for {
		i := strings.Index(out, key)
		if i < 0 {
			break
		}
		out = out[i+len(key):]
		j := strings.IndexByte(out, '"')
		if j < 0 {
			break
		}
		ids = append(ids, out[:j])
		out = out[j+1:]
	}
	return ids
}
