package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/httplog"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/oauth"
	"github.com/sopranoworks/shoka/pkg/reqtrace"
)

// B-57 — the LIVE-PATH proof the B-56 unit test could not give.
//
// B-56's unit test constructed reqtrace.Middleware(logger, surface, true) DIRECTLY,
// proving the dump branch works in isolation. But a live claude.ai connect after
// B-56 landed emitted NO `http request dump` / `http response dump` line: the dump
// never fired on the path requests actually travel. The cause was not the dump code
// (correct) but that `server.debug.dump_http` reached the live middleware as false —
// the flag was undocumented and its state invisible, so the running config never
// effectively set it (the B-53 "tests green, live silent" pattern: the test bypassed
// the config-load + wiring path where the silence lived).
//
// These tests close that gap by driving requests through the SAME two seams the live
// server uses — config.Load (the real YAML→flag parse) and tracedHandler (the real
// outermost wrap main.go applies to every listener) — instead of constructing the
// middleware by hand. A regression that breaks the parse or the threading makes these
// FAIL loud, where B-56's unit test stayed green.

// loadDumpConfig writes a minimal real config (dump on/off) to a temp file and loads
// it through config.Load — the exact parse path the running server uses.
func loadDumpConfig(t *testing.T, dumpHTTP bool) *config.Config {
	t.Helper()
	yaml := "" +
		"server:\n" +
		"  http:\n" +
		"    listen: \":8080\"\n" +
		"  mcp:\n" +
		"    oauth:\n" +
		"      listen: \":8082\"\n" +
		"  debug:\n" +
		"    dump_http: " + map[bool]string{true: "true", false: "false"}[dumpHTTP] + "\n" +
		"storage:\n" +
		"  base_dir: \"./data\"\n"
	path := filepath.Join(t.TempDir(), "shoka.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// runThroughTracedHandler wraps inner exactly as main.go does — tracedHandler with
// the flag read off the loaded config — then drives req and returns the raw log text
// plus the parsed JSON lines. This is the live wiring, not an isolated middleware.
func runThroughTracedHandler(t *testing.T, cfg *config.Config, surface string, inner http.Handler, req *http.Request) (string, []map[string]any) {
	t.Helper()
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h := tracedHandler(logger, surface, cfg.Server.Debug.DumpHTTP, inner)
	h.ServeHTTP(httptest.NewRecorder(), req)

	var lines []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if ln == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("log line not JSON: %q: %v", ln, err)
		}
		lines = append(lines, rec)
	}
	return buf.String(), lines
}

// The config-parse path: server.debug.dump_http: true in real YAML must reach the
// field the middleware reads. This is the candidate the live silence pointed at —
// pinned by a test, not assumed.
func TestB57_ConfigParse_DumpFlagReachesField(t *testing.T) {
	if got := loadDumpConfig(t, true).Server.Debug.DumpHTTP; got != true {
		t.Fatalf("server.debug.dump_http: true did not parse to cfg.Server.Debug.DumpHTTP (got %v)", got)
	}
	if got := loadDumpConfig(t, false).Server.Debug.DumpHTTP; got != false {
		t.Fatalf("server.debug.dump_http: false parsed as %v", got)
	}
}

// LIVE-PATH PROOF on the OAuth surface: a representative OAuth MCP request (valid
// bearer, JSON body) driven through the real oauth chain (httplog + the OAuth
// authenticator + oauthListenerHandler) wrapped by tracedHandler with the
// config-loaded flag emits BOTH dump lines with completeness (method, all headers,
// full body, status). B-59: the dump is now RAW — secrets appear VERBATIM and the
// «redacted» marker / Authorization fingerprint substitution never appear.
func TestB59_LivePath_OAuth_DumpFiresVerbatimNoRedaction(t *testing.T) {
	cfg := loadDumpConfig(t, true)

	const validToken = "LIVE-SECRET-TOKEN"
	oauthAuth := auth.New(auth.Config{
		ValidateToken: func(token string) (auth.Principal, auth.RejectReason, bool) {
			if token == validToken {
				return auth.Principal{ClientID: "live-client", Name: "live"}, "", true
			}
			return auth.Principal{}, auth.ReasonInvalidToken, false
		},
	})

	// Inner MCP handler: echoes a non-secret marker + a secret response field + a
	// custom header + an explicit status, so both dumps carry full, assertable content.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Mcp-Marker", "live-verbatim-value")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"ok","access_token":"SECRET-ACCESS"}`))
	})

	oauthInner := httplog.Middleware(slog.Default())(oauthListenerHandler(oauth.DiscoveryConfig{}, nil, inner, oauthAuth))

	// A representative OAuth MCP request: POST to the catch-all with a valid bearer and
	// a body that carries a (formerly redacted) code_verifier plus non-secret content —
	// B-59 dumps it verbatim.
	body := `{"jsonrpc":"2.0","method":"initialize","code_verifier":"SECRET-VERIFIER","client_id":"live-client"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+validToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Req-Marker", "req-verbatim-value")

	full, lines := runThroughTracedHandler(t, cfg, "mcp-oauth", oauthInner, req)

	reqDump := find(lines, "http request dump")
	respDump := find(lines, "http response dump")
	if reqDump == nil || respDump == nil {
		t.Fatalf("dump lines missing on the live oauth path (request=%v response=%v):\n%s", reqDump != nil, respDump != nil, full)
	}

	// Request completeness: method, the custom header (verbatim), the non-secret body.
	mustField(t, reqDump, "http request dump", "surface", "mcp-oauth")
	mustField(t, reqDump, "http request dump", "http_method", "POST")
	if h, _ := reqDump["headers"].(string); !strings.Contains(h, "X-Req-Marker: req-verbatim-value") {
		t.Errorf("request dump headers missing the verbatim custom header:\n%v", reqDump["headers"])
	}
	if b, _ := reqDump["body"].(string); !strings.Contains(b, `"client_id":"live-client"`) {
		t.Errorf("request dump body missing non-secret content:\n%v", reqDump["body"])
	}

	// Response completeness: status, the custom header (verbatim), the non-secret body.
	mustField(t, respDump, "http response dump", "status", float64(200))
	if h, _ := respDump["headers"].(string); !strings.Contains(h, "X-Mcp-Marker: live-verbatim-value") {
		t.Errorf("response dump headers missing the verbatim custom header:\n%v", respDump["headers"])
	}
	if b, _ := respDump["body"].(string); !strings.Contains(b, `"result":"ok"`) {
		t.Errorf("response dump body missing non-secret content:\n%v", respDump["body"])
	}

	// B-59: NO redaction — every secret is dumped VERBATIM (the request bearer + the
	// request-body code_verifier + the response access_token), the Authorization header
	// appears in clear, and neither the «redacted» marker nor a fingerprint substitution
	// appears.
	for _, secret := range []string{validToken, "SECRET-VERIFIER", "SECRET-ACCESS"} {
		if !strings.Contains(full, secret) {
			t.Errorf("expected secret %q dumped VERBATIM on the live path:\n%s", secret, full)
		}
	}
	if !strings.Contains(full, "Authorization: Bearer "+validToken) {
		t.Errorf("Authorization header not dumped verbatim:\n%s", full)
	}
	if strings.Contains(full, "«redacted»") {
		t.Errorf("redaction marker present — the dump must be raw:\n%s", full)
	}
	if strings.Contains(full, "fingerprint=") {
		t.Errorf("Authorization fingerprint substitution present — the dump must be raw:\n%s", full)
	}
}

// All three surfaces fire the dump on the live wiring (web / mcp-plain / mcp-oauth).
func TestB57_LivePath_AllThreeSurfaces_DumpFires(t *testing.T) {
	cfg := loadDumpConfig(t, true)

	cases := []struct {
		surface string
		inner   http.Handler
		req     *http.Request
	}{
		{
			surface: "web",
			inner: func() http.Handler {
				a := auth.New(auth.Config{}) // disabled → serves through
				mux := http.NewServeMux()
				mux.Handle("/api/", a.Middleware(reqtrace.Route("web-api", okHandler())))
				return mux
			}(),
			req: httptest.NewRequest(http.MethodGet, "/api/project/status", nil),
		},
		{
			surface: "mcp-plain",
			inner: httplog.Middleware(slog.Default())(
				auth.New(auth.Config{}).Middleware(reqtrace.Route("mcp-dispatch", okHandler()))),
			req: httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}")),
		},
		{
			surface: "mcp-oauth",
			inner: httplog.Middleware(slog.Default())(oauthListenerHandler(
				oauth.DiscoveryConfig{}, nil,
				auth.New(auth.Config{}).Middleware(okHandler()),
				auth.New(auth.Config{}))),
			req: httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil),
		},
	}

	for _, c := range cases {
		t.Run(c.surface, func(t *testing.T) {
			full, lines := runThroughTracedHandler(t, cfg, c.surface, c.inner, c.req)
			reqDump := find(lines, "http request dump")
			respDump := find(lines, "http response dump")
			if reqDump == nil || respDump == nil {
				t.Fatalf("dump did not fire on surface %s (request=%v response=%v):\n%s",
					c.surface, reqDump != nil, respDump != nil, full)
			}
			mustField(t, reqDump, "http request dump", "surface", c.surface)
			mustField(t, respDump, "http response dump", "surface", c.surface)
		})
	}
}

// The live symptom reproduced: with the flag OFF (the running state that produced the
// silence), the dump lines are absent though reqtrace's `request received` still fires
// — exactly what the live log showed. This is the test that would have caught B-56's
// silence had it existed.
func TestB57_LivePath_DumpOff_SilentButTraceStillFires(t *testing.T) {
	cfg := loadDumpConfig(t, false)
	full, lines := runThroughTracedHandler(t, cfg, "mcp-oauth",
		httplog.Middleware(slog.Default())(okHandler()),
		httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if find(lines, "http request dump") != nil || find(lines, "http response dump") != nil {
		t.Fatalf("dump fired while server.debug.dump_http is OFF:\n%s", full)
	}
	if find(lines, "request received") == nil {
		t.Fatalf("reqtrace `request received` should still fire with the dump OFF:\n%s", full)
	}
}
