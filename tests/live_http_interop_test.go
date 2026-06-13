package tests

// Live, wire-level interoperability tests for Shoka's MCP transport.
//
// These tests run the REAL Shoka binary as a separate process bound to an
// ephemeral TCP port, then drive it with the SDK's own MCP client over the
// Streamable HTTP transport — exactly the protocol path a third-party client
// (e.g. Claude Code, via `claude mcp add --transport http`) follows. They
// deliberately encode NO prior knowledge of Shoka's tool catalog: tools are
// discovered via tools/list and a no-argument tool is selected at runtime.
// Assertions are limited to "the protocol round-trip worked" (IsError==false,
// content non-empty), never specific field names or values.
//
// See meta/directives/2026-05-29-shoka-http-transport.md §2.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpPath is the documented Streamable HTTP endpoint path (see
// docs/contracts/mcp-v1.md § Transport). The SDK handler is path-agnostic on the
// dedicated MCP listener, but every test drives this canonical path.
const mcpPath = "/mcp"

// builtServerBin is the path to the Shoka binary built once for all live tests.
var builtServerBin string

func TestMain(m *testing.M) {
	// Build the real binary once; every live test execs this same artifact so we
	// exercise the actual startup, config loading, and listener wiring.
	dir, err := os.MkdirTemp("", "shoka-live-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "live interop: mkdtemp: %v\n", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "shoka-server")
	build := exec.Command("go", "build", "-o", bin, "./cmd/shoka")
	build.Dir = ".." // repo root, relative to the tests/ package dir
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "live interop: build failed: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	builtServerBin = bin

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// freePort returns a currently-free TCP port on the loopback interface. There is
// an inherent TOCTOU race between closing the listener and the server re-binding,
// but it is acceptable for a local test and keeps the test self-contained.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// mcpEndpoint builds the canonical Streamable HTTP endpoint URL for a port.
func mcpEndpoint(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, mcpPath)
}

// writeLiveConfig writes a minimal but complete Shoka config and returns its path.
// authEnabled drives BOTH the plain MCP port's static-bearer enforcement
// (server.mcp.plain.bearer_auth, the B-50 per-port auth source) and the Web/non-MCP
// token policy (server.auth.enabled), so a single flag keeps the long-standing
// "auth on → MCP requires the token" intent of the live suite after the two-transport
// split. The single plain transport carries the documented /mcp endpoint.
func writeLiveConfig(t *testing.T, baseDir string, httpPort, mcpPort int, level string, authEnabled bool, token string) string {
	t.Helper()
	tokensYAML := "[]"
	if authEnabled && token != "" {
		tokensYAML = fmt.Sprintf("[%q]", token)
	}
	cfg := fmt.Sprintf(`server:
  http:
    listen: "127.0.0.1:%d"
    external_url: "http://127.0.0.1:%d"
  mcp:
    plain:
      listen: "127.0.0.1:%d"
      external_url: "http://127.0.0.1:%d"
      bearer_auth: %t
  auth:
    enabled: %t
    tokens: %s
    allowed_origins: []
  log:
    level: %q
    format: "text"
storage:
  base_dir: %q
services:
  google_cloud:
    project_id: ""
`, httpPort, httpPort, mcpPort, mcpPort, authEnabled, authEnabled, tokensYAML, level, baseDir)

	path := filepath.Join(t.TempDir(), "shoka.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writeLiveConfig: %v", err)
	}
	return path
}

// startLiveServer starts the built binary with the given config, captures its
// stderr/stdout to logPath, waits until the MCP port accepts connections, and
// returns a cleanup func that terminates the process.
func startLiveServer(t *testing.T, cfgPath, logPath string, mcpPort int) func() {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("startLiveServer: create log: %v", err)
	}
	cmd := exec.Command(builtServerBin, "-config", cfgPath)
	cmd.Stderr = logFile
	cmd.Stdout = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("startLiveServer: start: %v", err)
	}

	cleanup := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
		logFile.Close()
	}

	if !waitMCPPort(mcpPort, 10*time.Second) {
		cleanup()
		t.Fatalf("startLiveServer: MCP port %d never came up (see %s)", mcpPort, logPath)
	}
	return cleanup
}

// waitMCPPort polls the loopback MCP port until it accepts a connection or the
// timeout elapses.
func waitMCPPort(mcpPort int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", mcpPort)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// hasNoRequiredArgs reports whether a tool's input schema declares no required
// fields, i.e. the tool is callable with empty arguments. InputSchema is decoded
// JSON (SDK v1.6.0 types it as `any`), so we inspect the raw object rather than a
// typed schema. A missing/non-object schema or absent "required" array means
// "no required args".
func hasNoRequiredArgs(schema any) bool {
	m, ok := schema.(map[string]any)
	if !ok {
		return true
	}
	req, ok := m["required"]
	if !ok {
		return true
	}
	arr, ok := req.([]any)
	if !ok {
		return true
	}
	return len(arr) == 0
}

// runLifecycle performs one full MCP lifecycle over the wire: connect (which
// performs the initialize handshake), discover tools, pick a no-argument tool,
// and call it. It asserts only that the round-trip succeeded.
func runLifecycle(t *testing.T, mcpURL string, httpClient *http.Client, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	transport := &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "interop-test", Version: "0.0.1"}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("[%s] Connect (initialize handshake) failed: %v", phase, err)
	}
	defer session.Close()

	lt, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("[%s] ListTools failed: %v", phase, err)
	}
	if len(lt.Tools) == 0 {
		t.Fatalf("[%s] ListTools returned no tools", phase)
	}

	// Select the first tool callable with empty arguments (no required fields).
	var picked string
	for _, tool := range lt.Tools {
		if hasNoRequiredArgs(tool.InputSchema) {
			picked = tool.Name
			break
		}
	}
	if picked == "" {
		t.Fatalf("[%s] no no-argument tool available among %d tools; "+
			"wire smoke test needs at least one (this is itself a finding)", phase, len(lt.Tools))
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: picked})
	if err != nil {
		t.Fatalf("[%s] CallTool(%q) failed: %v", phase, picked, err)
	}
	if res.IsError {
		t.Fatalf("[%s] CallTool(%q) returned IsError=true", phase, picked)
	}
	if len(res.Content) == 0 {
		t.Fatalf("[%s] CallTool(%q) returned empty content", phase, picked)
	}
}

func TestLiveHTTPInterop_NoAuth(t *testing.T) {
	for _, level := range []string{"info", "debug"} {
		level := level
		t.Run(level, func(t *testing.T) {
			httpPort := freePort(t)
			mcpPort := freePort(t)
			baseDir := t.TempDir()
			cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, level, false, "")
			logPath := filepath.Join(t.TempDir(), "server.log")
			cleanup := startLiveServer(t, cfgPath, logPath, mcpPort)
			defer cleanup()

			mcpURL := mcpEndpoint(mcpPort)

			// First lifecycle.
			runLifecycle(t, mcpURL, nil, "first")
			// Reconnect against the same process and repeat (fresh session id).
			runLifecycle(t, mcpURL, nil, "reconnect")
		})
	}
}

func TestLiveHTTPInterop_Auth(t *testing.T) {
	const token = "live-interop-secret-token"
	httpPort := freePort(t)
	mcpPort := freePort(t)
	baseDir := t.TempDir()
	cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", true, token)
	logPath := filepath.Join(t.TempDir(), "server.log")
	cleanup := startLiveServer(t, cfgPath, logPath, mcpPort)
	defer cleanup()

	mcpURL := mcpEndpoint(mcpPort)
	// Reuse bearerRT (defined in logging_secret_test.go) to carry the token, since
	// StreamableClientTransport exposes no header field — only a custom http.Client.
	httpClient := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}

	runLifecycle(t, mcpURL, httpClient, "auth-first")
	runLifecycle(t, mcpURL, httpClient, "auth-reconnect")
}
