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
	"strings"
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

// freePort returns a currently-free TCP port on the loopback interface by binding
// :0, reading the assigned port, and releasing the listener. There is an
// unavoidable window between releasing the listener here and the server re-binding
// the port, during which a concurrent binder (another package's harness under the
// whole-module `go test -race -count=30 ./...` gate) can steal it. That
// close-before-rebind TOCTOU is closed STRUCTURALLY by startLiveServer, which
// detects the resulting bind-failure process exit and relaunches on a freshly
// chosen port (see startLiveServer / awaitReady) — never by widening this window
// or sleeping.
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

// maxLaunchAttempts bounds the retry that closes the freePort close-before-rebind
// TOCTOU: when a launched server exits during startup because a chosen port was
// stolen (a logged bind failure), the harness relaunches on freshly chosen ports
// up to this many times. A server that dies for any OTHER reason fails fast (no
// retry) and one that hangs while alive is never retried — so the retry can only
// absorb an actual port steal, never a real defect.
const maxLaunchAttempts = 6

// liveReadyTimeout bounds how long the harness waits for a launched server to begin
// serving. It is deliberately generous: under the whole-module
// `go test -race -count=30 ./...` gate ~16 packages build and run at once and a
// `-race` binary can take many seconds to start on a CPU-saturated host. It is NOT
// a tuned timing knob (B-29) — readiness is decided by the real post-condition (the
// ports accept) and process death is surfaced immediately, so this only caps a
// genuine hang rather than standing in for the readiness signal.
const liveReadyTimeout = 60 * time.Second

// liveProc is a launched Shoka server process whose cmd.Wait is owned by a single
// background goroutine. That single owner lets readiness polling observe liveness
// (alive) without racing the reaper that stop() depends on.
type liveProc struct {
	cmd     *exec.Cmd
	logFile *os.File
	logPath string
	waited  chan struct{} // closed when the background cmd.Wait() returns
}

// launchProc starts the built binary with cfgPath, tees stdout+stderr to logPath,
// and starts the sole cmd.Wait() reaper. It does NOT wait for readiness.
func launchProc(t *testing.T, cfgPath, logPath string) *liveProc {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("launchProc: create log: %v", err)
	}
	cmd := exec.Command(builtServerBin, "-config", cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("launchProc: start: %v", err)
	}
	p := &liveProc{cmd: cmd, logFile: logFile, logPath: logPath, waited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.waited) }()
	return p
}

// alive reports whether the process has not yet exited.
func (p *liveProc) alive() bool {
	select {
	case <-p.waited:
		return false
	default:
		return true
	}
}

// stop signals the server to exit, waits for the reaper up to a grace period, then
// force-kills if needed, and closes the log file. Safe to call exactly once.
func (p *liveProc) stop() {
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.waited:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.waited
	}
	p.logFile.Close()
}

// allAccept reports whether every loopback port accepts a connection right now.
func allAccept(ports []int) bool {
	for _, port := range ports {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
	}
	return true
}

// awaitReady polls until every port in ports accepts a connection (the server is
// fully serving) or p exits (a crashed / bind-failed start), whichever happens
// first, bounded by liveReadyTimeout. ready is true iff all ports accepted; died is
// true iff the process exited before that. Both false means the generous bound
// elapsed while the process was still alive — a genuine hang, not a port steal.
//
// Awaiting EVERY configured listener (not just one) is what makes a bind steal on
// any of a server's ports trigger the retry: a single process exits on any
// listener's bind failure, and waiting on all of them avoids proceeding in the
// brief window where one port is briefly accepting while the process is already
// tearing down because another failed to bind.
func awaitReady(p *liveProc, ports []int) (ready, died bool) {
	deadline := time.Now().Add(liveReadyTimeout)
	for time.Now().Before(deadline) {
		if allAccept(ports) {
			return true, false
		}
		if !p.alive() {
			return false, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, false
}

// looksLikeBindFailure reports whether the server log records a listener bind
// failure (a stolen port). It distinguishes a port steal — which startLiveServer
// retries on a fresh port — from any other startup death, which must NOT be masked.
func looksLikeBindFailure(logPath string) bool {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return false
	}
	s := string(data)
	return strings.Contains(s, "address already in use") || strings.Contains(s, "bind:")
}

// liveLaunch is one launch attempt's specification: the config to run, where its
// log goes, and the ports whose readiness is awaited (every listener the config
// opens). A fresh one — with freshly chosen ports — is produced per attempt so a
// stolen port is retried, not failed.
type liveLaunch struct {
	cfgPath    string
	logPath    string
	readyPorts []int
}

// startLiveServer launches the built binary and waits until it is actually serving
// — every configured port accepts a connection AND the process is alive — then
// returns a cleanup func that terminates it. build is invoked once per attempt and
// must allocate fresh ports, write the config, and return the launch spec; callers
// capture the chosen ports via the closure so a retry's fresh ports are visible.
//
// This addresses both harness root causes named in the 2026-06-13 live-harness
// robustness directive: (1) the freePort close-before-rebind TOCTOU is closed
// structurally — a server that exits during startup with a bind failure in its log
// (a stolen port) is relaunched on freshly chosen ports, up to maxLaunchAttempts;
// (2) the former fixed 10s readiness deadline is replaced by awaiting the real
// post-condition with a generous-under-load bound, with process death surfaced
// immediately rather than waited out. No synthetic timing, no sleep band-aid: a
// non-bind startup death or a genuine hang fails fast with the server log.
func startLiveServer(t *testing.T, build func() liveLaunch) func() {
	t.Helper()
	for attempt := 1; attempt <= maxLaunchAttempts; attempt++ {
		spec := build()
		p := launchProc(t, spec.cfgPath, spec.logPath)
		ready, died := awaitReady(p, spec.readyPorts)
		if ready {
			return p.stop
		}
		p.stop()
		if died && looksLikeBindFailure(spec.logPath) {
			t.Logf("startLiveServer: attempt %d/%d: server exited with a bind failure on "+
				"ports %v (stolen port); relaunching on fresh ports", attempt, maxLaunchAttempts, spec.readyPorts)
			continue
		}
		if died {
			t.Fatalf("startLiveServer: server exited during startup without a bind failure "+
				"(a real defect, not a port steal); see %s", spec.logPath)
		}
		t.Fatalf("startLiveServer: ports %v never came up within %s while the server stayed "+
			"alive (a genuine hang, not a port steal); see %s", spec.readyPorts, liveReadyTimeout, spec.logPath)
	}
	t.Fatalf("startLiveServer: server could not bind a free port after %d attempts "+
		"(persistent port contention)", maxLaunchAttempts)
	return func() {}
}

// serverStartedListener reports whether the launched server's captured startup
// log shows it began serving the named transport. runServer logs
// `msg="starting server" name=<name> addr=…` exactly once per listener it opens
// (MCP-plain / MCP-oauth / Web), and never for a transport whose listen address
// is absent from the config.
//
// This is the RACE-FREE replacement for asserting a NEGATIVE via portBound on a
// port the test does not own. A "must NOT bind" check that picks a free port,
// releases it, and probes that nothing is listening is unsound under the
// whole-module `go test -race -count=30 ./...` gate: between freePort releasing
// the port and the probe, another package's harness can grab and hold that exact
// ephemeral port, flipping the probe true. (c6d0e97 hardened only the must-BIND
// direction — relaunching when OUR server loses a chosen port — and deliberately
// left waitMCPPort as the bare must/must-not probe; the must-not direction is the
// residual race this closes.) The server's own log is authoritative about which
// listeners it opened and owns no contended port, so the signal is deterministic.
//
// Ordering is safe: each listener's line is logged in runServer's goroutine
// immediately before its ListenAndServe, and slog writes straight to the log
// file (no userspace buffering), so by the time startLiveServer has returned
// (every configured port accepts) every opened listener's line is on disk, and
// an un-opened listener's line is permanently absent.
func serverStartedListener(t *testing.T, logPath, name string) bool {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("serverStartedListener: read log %s: %v", logPath, err)
	}
	return strings.Contains(string(data), `msg="starting server" name=`+name+" ")
}

// waitMCPPort polls a loopback TCP port until it accepts a connection or the
// timeout elapses. It is a bare port-presence probe used by the two-transport tests
// to assert that a configured port DOES bind (portBound) — a POSITIVE check on a
// port THIS test owns (the server holds it for the test's lifetime), so it is
// race-free. The NEGATIVE "must not bind" direction is NOT done this way — see
// serverStartedListener — because a port the test does not own can be transiently
// bound by another package under the whole-module gate. It is deliberately NOT the
// server-readiness path — that is awaitReady, which also watches the child process
// so a crashed start is detected immediately instead of waited out.
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

	// B-31 phase 3 registration guard: the real server must advertise the History
	// tools get_history and its diff sibling get_diff, so agents can list and diff
	// versions over MCP. Asserted on the existing ListTools result (no extra
	// round-trip).
	advertised := make(map[string]bool, len(lt.Tools))
	for _, tool := range lt.Tools {
		advertised[tool.Name] = true
	}
	for _, must := range []string{"get_history", "get_diff"} {
		if !advertised[must] {
			t.Fatalf("[%s] advertised tool set is missing %q (have %d tools)", phase, must, len(lt.Tools))
		}
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
			baseDir := t.TempDir()
			var mcpPort int
			cleanup := startLiveServer(t, func() liveLaunch {
				httpPort := freePort(t)
				mcpPort = freePort(t)
				cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, level, false, "")
				logPath := filepath.Join(t.TempDir(), "server.log")
				return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, mcpPort}}
			})
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
	baseDir := t.TempDir()
	var mcpPort int
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort := freePort(t)
		mcpPort = freePort(t)
		cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", true, token)
		logPath := filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, mcpPort}}
	})
	defer cleanup()

	mcpURL := mcpEndpoint(mcpPort)
	// Reuse bearerRT (defined in logging_secret_test.go) to carry the token, since
	// StreamableClientTransport exposes no header field — only a custom http.Client.
	httpClient := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}

	runLifecycle(t, mcpURL, httpClient, "auth-first")
	runLifecycle(t, mcpURL, httpClient, "auth-reconnect")
}
