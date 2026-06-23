package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/httplog"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/oauth"
	"github.com/sopranoworks/shoka/pkg/reqtrace"
)

// B-53 §2.6 — prove no path is exempt: a single correlated entry-to-exit trace
// (one request_id shared across the entry, auth, and response lines) exists for
// representative requests on ALL THREE listeners (plain MCP, OAuth MCP, Web) and
// for BOTH success and rejection — including the exact live failing case: a
// token-bearing MCP initialize that 401s on path=/ must trace to port (surface),
// raw path, auth result+reason, routing, and status, all under one id.

// buildAndRun wires `surface` tracing + inner middleware around `inner`, runs `req`,
// and returns the lines sharing the one request_id.
func buildAndRun(t *testing.T, surface string, wrap func(*slog.Logger, http.Handler) http.Handler, inner http.Handler, req *http.Request) []map[string]any {
	t.Helper()
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := reqtrace.Middleware(logger, surface, false)(wrap(logger, inner))
	h.ServeHTTP(httptest.NewRecorder(), req)

	var lines []map[string]any
	ids := map[string]bool{}
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line not JSON: %q: %v", ln, err)
		}
		lines = append(lines, rec)
		if id, ok := rec["request_id"].(string); ok && id != "" {
			ids[id] = true
		}
	}
	if len(ids) != 1 {
		t.Fatalf("expected exactly one correlation id across the trace, got %d (%v)\nlines: %v", len(ids), ids, lines)
	}
	// Secret-leak guard on every trace (directive §5): no credential value anywhere.
	full := buf.String()
	for _, secret := range []string{"SECRET-TOKEN", "SECRET-STATIC"} {
		if strings.Contains(full, secret) {
			t.Fatalf("secret %q leaked into the trace:\n%s", secret, full)
		}
	}
	return lines
}

// find returns the first line whose msg == m, or nil.
func find(lines []map[string]any, m string) map[string]any {
	for _, l := range lines {
		if l["msg"] == m {
			return l
		}
	}
	return nil
}

func mustField(t *testing.T, line map[string]any, msg, key string, want any) {
	t.Helper()
	if line == nil {
		t.Fatalf("missing line %q", msg)
	}
	if line[key] != want {
		t.Errorf("%s.%s: got %v want %v", msg, key, line[key], want)
	}
}

// okHandler (a 200 handler) is shared with main_test.go in this package.

// --- The live failing case: token-bearing MCP initialize that 401s on path=/ -----

func TestB53_OAuthListener_TokenBearingInitialize401OnRoot_FullyTraced(t *testing.T) {
	// The catch-all "/" is tagged mcp-dispatch INSIDE auth, so a pre-routing 401 (the
	// live symptom: OAuth succeeded earlier but this MCP request's token is not
	// accepted here) must show route=unrouted. The validator rejects as invalid-token.
	wrap := func(logger *slog.Logger, inner http.Handler) http.Handler {
		a := auth.New(auth.Config{
			Logger: logger,
			ValidateToken: func(token string) (auth.Principal, auth.RejectReason, bool) {
				return auth.Principal{}, auth.ReasonInvalidToken, false
			},
		})
		return httplog.Middleware(logger)(oauthListenerHandler(oauth.DiscoveryConfig{Logger: logger}, nil, inner, a))
	}

	req := httptest.NewRequest(http.MethodPost, "/", nil) // path=/ — the proxy-rewritten target
	req.Header.Set("Authorization", "Bearer SECRET-TOKEN")
	req.Header.Set("Content-Type", "application/json")
	// No Mcp-Session-Id → this is the initialize handshake.

	lines := buildAndRun(t, "mcp-oauth", wrap, okHandler(), req)

	entry := find(lines, "request received")
	mustField(t, entry, "request received", "surface", "mcp-oauth")
	mustField(t, entry, "request received", "path", "/")
	mustField(t, entry, "request received", "authorization_present", true)
	mustField(t, entry, "request received", "mcp_session_id_present", false)

	rejected := find(lines, "auth rejected")
	mustField(t, rejected, "auth rejected", "authenticator", "oauth")
	mustField(t, rejected, "auth rejected", "reason", "invalid-token")

	completed := find(lines, "request completed")
	mustField(t, completed, "request completed", "surface", "mcp-oauth")
	mustField(t, completed, "request completed", "route", "unrouted") // 401'd BEFORE routing
	mustField(t, completed, "request completed", "status", float64(401))
	mustField(t, completed, "request completed", "reason", "unauthorized")
}

// --- Plain MCP listener: success (disabled) + reject (static bearer) -------------

func plainWrap(enabled bool) func(*slog.Logger, http.Handler) http.Handler {
	return func(logger *slog.Logger, inner http.Handler) http.Handler {
		a := auth.New(auth.Config{Logger: logger, Enabled: enabled, Tokens: []string{"SECRET-STATIC"}})
		return httplog.Middleware(logger)(a.Middleware(reqtrace.Route("mcp-dispatch", inner)))
	}
}

func TestB53_PlainListener_Success_FullyTraced(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil) // initialize handshake
	lines := buildAndRun(t, "mcp-plain", plainWrap(false), okHandler(), req)

	mustField(t, find(lines, "request received"), "request received", "surface", "mcp-plain")
	mustField(t, find(lines, "auth ok"), "auth ok", "authenticator", "disabled")
	completed := find(lines, "request completed")
	mustField(t, completed, "request completed", "route", "mcp-dispatch")
	mustField(t, completed, "request completed", "status", float64(200))
}

func TestB53_PlainListener_Reject_FullyTraced(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil) // no bearer → missing-bearer
	lines := buildAndRun(t, "mcp-plain", plainWrap(true), okHandler(), req)

	mustField(t, find(lines, "auth rejected"), "auth rejected", "reason", "missing-bearer")
	completed := find(lines, "request completed")
	mustField(t, completed, "request completed", "route", "unrouted")
	mustField(t, completed, "request completed", "status", float64(401))
}

// --- Web listener: success + reject (static-bearer /api/ route) ------------------

func webWrap(enabled bool) func(*slog.Logger, http.Handler) http.Handler {
	return func(logger *slog.Logger, inner http.Handler) http.Handler {
		a := auth.New(auth.Config{Logger: logger, Enabled: enabled, Tokens: []string{"SECRET-STATIC"}})
		mux := http.NewServeMux()
		mux.Handle("/api/", a.Middleware(reqtrace.Route("web-api", inner)))
		return mux // the Web listener is NOT httplog-wrapped — reqtrace is its only outer layer
	}
}

func TestB53_WebListener_Success_FullyTraced(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/project/status", nil)
	lines := buildAndRun(t, "web", webWrap(false), okHandler(), req)

	mustField(t, find(lines, "request received"), "request received", "surface", "web")
	mustField(t, find(lines, "auth ok"), "auth ok", "authenticator", "disabled")
	completed := find(lines, "request completed")
	mustField(t, completed, "request completed", "route", "web-api")
	mustField(t, completed, "request completed", "status", float64(200))
}

func TestB53_WebListener_Reject_FullyTraced(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/project/status", nil) // no bearer
	lines := buildAndRun(t, "web", webWrap(true), okHandler(), req)

	mustField(t, find(lines, "request received"), "request received", "surface", "web")
	rejected := find(lines, "auth rejected")
	mustField(t, rejected, "auth rejected", "authenticator", "static-bearer")
	mustField(t, rejected, "auth rejected", "reason", "missing-bearer")
	completed := find(lines, "request completed")
	mustField(t, completed, "request completed", "route", "unrouted")
	mustField(t, completed, "request completed", "status", float64(401))
}
