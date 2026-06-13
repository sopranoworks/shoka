package tests

// Server-restart recovery: the exact failure that motivated the SSE → Streamable
// HTTP migration (meta/directives/2026-05-29-shoka-http-transport.md §0, §2.5).
//
// Scenario:
//  1. Start Shoka process #1. Connect an SDK client. Call a tool — succeeds.
//  2. Kill process #1 (SIGTERM, wait for exit). The SDK client stays alive.
//  3. Start process #2 on the SAME port (a fresh server with an empty session map).
//  4. The stale client issues a tool call. The server does not recognize its
//     Mcp-Session-Id, so per the spec it MUST return 404 Not Found. The SDK
//     surfaces that as mcp.ErrSessionMissing — NOT the old SSE-era failure
//     `method "tools/call" is invalid during session initialization`.
//  5. Recovery: a fresh client + transport re-initializes against process #2 and
//     the tool call succeeds.
//
// The whole run is at log.level=debug; process #2's log is read back and the
// 404 issuance + re-initialize + successful recovery call are asserted and
// surfaced for the completion report.
//
// Spec basis: "The server MAY terminate the session at any time, after which it
// MUST respond to requests containing that session ID with HTTP 404 Not Found"
// — https://modelcontextprotocol.io/specification/2025-03-26/basic/transports
// SDK basis: a 404 on a known-but-unrecognized session id wraps
// mcp.ErrSessionMissing (go-sdk@v1.6.0/mcp/transport.go:35-37,
// streamable.go:295-301).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startProc starts the built binary with cfgPath, awaits readiness on every port
// in ports (port-accepts AND process-alive, generous bound — the shared awaitReady
// path, NOT a fixed deadline), and returns the running proc. ok is false ONLY when
// the process exited with a bind failure during startup (a stolen reused port),
// signalling the caller to retry the whole scenario on fresh ports; a non-bind
// startup death or a genuine hang is a hard t.Fatalf, never masked.
func startProc(t *testing.T, cfgPath, logPath string, ports ...int) (*liveProc, bool) {
	t.Helper()
	p := launchProc(t, cfgPath, logPath)
	ready, died := awaitReady(p, ports)
	if ready {
		return p, true
	}
	p.stop()
	if died && looksLikeBindFailure(logPath) {
		return nil, false
	}
	if died {
		t.Fatalf("startProc: server exited during startup without a bind failure "+
			"(a real defect, not a port steal); see %s", logPath)
	}
	t.Fatalf("startProc: ports %v never came up within %s while the server stayed alive "+
		"(a genuine hang, not a port steal); see %s", ports, liveReadyTimeout, logPath)
	return nil, false
}

// callFirstNoArgTool discovers a no-argument tool on a fresh session and calls
// it, returning the tool name. It is the "tool works" probe for a live server.
func callFirstNoArgTool(ctx context.Context, t *testing.T, sess *mcp.ClientSession) string {
	t.Helper()
	noArg := discoverNoArgTools(ctx, t, sess)
	if len(noArg) == 0 {
		t.Fatalf("no no-argument tool discovered; cannot probe the server")
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: noArg[0]})
	if err != nil {
		t.Fatalf("probe CallTool(%q) failed: %v", noArg[0], err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("probe CallTool(%q): IsError=%v content=%d", noArg[0], res.IsError, len(res.Content))
	}
	return noArg[0]
}

func TestLiveHTTPServerRestart(t *testing.T) {
	// Both server processes deliberately bind the SAME reused port, so a fresh-port
	// retry (the freePort close-before-rebind TOCTOU fix) must restart the WHOLE
	// scenario, not an individual launch. runRestartScenario returns false only when
	// a launch exits with a bind failure (a stolen reused port) before any
	// assertion; loop on fresh ports up to maxLaunchAttempts.
	for attempt := 1; attempt <= maxLaunchAttempts; attempt++ {
		if runRestartScenario(t, attempt) {
			return
		}
		t.Logf("restart scenario attempt %d/%d: a server exited with a bind failure "+
			"(stolen reused port); retrying the whole scenario on fresh ports", attempt, maxLaunchAttempts)
	}
	t.Fatalf("restart scenario: could not secure a stable reused port after %d attempts "+
		"(persistent port contention)", maxLaunchAttempts)
}

// runRestartScenario runs the full server-restart scenario once. It returns true
// when the scenario ran to completion (every assertion executed — any failure
// already raised via t.Fatalf), and false ONLY when a server process exited with a
// bind failure during startup (a stolen reused port) before any assertion, telling
// the caller to retry on fresh ports. Because both processes share one port, that
// retry must redo the whole scenario rather than re-pick a port mid-flight.
func runRestartScenario(t *testing.T, attempt int) bool {
	t.Helper()
	// Reserve the ports for this attempt; both server processes bind the same MCP
	// port so the stale client reconnects to the new process exactly as Claude Code
	// would.
	httpPort := freePort(t)
	mcpPort := freePort(t)
	baseDir := t.TempDir() // shared data dir so the tool catalog is identical across restarts
	cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", false, "")
	mcpURL := mcpEndpoint(mcpPort)

	logPath1 := filepath.Join(t.TempDir(), "server1.log")
	logPath2 := filepath.Join(t.TempDir(), "server2.log")

	// --- Step 1: server #1 up, stale client connects and a tool works. ---
	p1, ok := startProc(t, cfgPath, logPath1, httpPort, mcpPort)
	if !ok {
		return false // bind-failure on a stolen port → retry the scenario on fresh ports
	}
	t.Logf("attempt %d: server #1 started (pid %d), log: %s", attempt, p1.cmd.Process.Pid, logPath1)

	// The client/RPC budget is created after #1 is serving so a generous (-race,
	// saturated-host) startup never eats into it. It must span the #2 startup + the
	// recovery RPCs that follow.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// DisableStandaloneSSE keeps the stale client to request/response POSTs only,
	// so after the restart its tool-call POST deterministically hits process #2
	// and gets the 404 — no background reconnect noise to race with.
	staleTransport := &mcp.StreamableClientTransport{Endpoint: mcpURL, DisableStandaloneSSE: true}
	staleCli := mcp.NewClient(&mcp.Implementation{Name: "restart-stale-client", Version: "0.0.1"}, nil)
	staleSess, err := staleCli.Connect(ctx, staleTransport, nil)
	if err != nil {
		p1.stop()
		t.Fatalf("stale client Connect to server #1 failed: %v", err)
	}
	probeTool := callFirstNoArgTool(ctx, t, staleSess)
	t.Logf("server #1: tool %q succeeded on the stale session", probeTool)

	// --- Step 2: kill server #1, wait for exit (releases the port). ---
	p1.stop()
	t.Logf("server #1 terminated")

	// --- Step 3: server #2 up on the SAME port (fresh, empty session map). ---
	p2, ok := startProc(t, cfgPath, logPath2, httpPort, mcpPort)
	if !ok {
		_ = staleSess.Close()
		return false // the just-freed reused port was stolen → retry the scenario
	}
	defer p2.stop()
	t.Logf("server #2 started (pid %d) on the same port, log: %s", p2.cmd.Process.Pid, logPath2)

	// --- Step 4: stale client's next tool call must hit a clean 404, surfaced as
	// ErrSessionMissing — never the old "invalid during session initialization". ---
	_, callErr := staleSess.CallTool(ctx, &mcp.CallToolParams{Name: probeTool})
	if callErr == nil {
		t.Fatalf("expected the stale-session tool call to fail after restart, but it succeeded")
	}
	t.Logf("stale-session tool call after restart errored as expected: %v", callErr)
	if strings.Contains(callErr.Error(), "invalid during session initialization") {
		t.Fatalf("REGRESSION: the SSE-era failure reappeared: %v", callErr)
	}
	if !errors.Is(callErr, mcp.ErrSessionMissing) {
		// Not fatal on its own (the directive accepts any clean-404-driven failure),
		// but record it: the server-log assertion below is the authoritative check.
		t.Logf("note: stale-call error does not wrap mcp.ErrSessionMissing (got %T: %v); "+
			"relying on the server's 404 log line for confirmation", callErr, callErr)
	}
	_ = staleSess.Close()

	// --- Step 5: recovery — a fresh client re-initializes against #2 and works. ---
	recoverSess := connectClient(ctx, t, mcpURL, nil, "restart-recovery-client")
	defer recoverSess.Close()
	recoverTool := callFirstNoArgTool(ctx, t, recoverSess)
	t.Logf("recovery: fresh client re-initialized and tool %q succeeded against server #2", recoverTool)

	// --- Assert the server-side evidence: process #2 issued a clean 404 for the
	// stale session id, then logged the re-initialize and the successful call. ---
	var log2Text string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, rerr := os.ReadFile(logPath2)
		if rerr != nil {
			t.Fatalf("read server #2 log: %v", rerr)
		}
		log2Text = string(data)
		if strings.Contains(log2Text, "status=404") && strings.Contains(log2Text, "mcp session established") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// B-53: the stale-session 404 is now surfaced on reqtrace's response-stage line
	// ("request completed", WARN on non-2xx) — which supersedes httplog's removed
	// "request rejected" line and additionally carries the correlation request_id, a
	// reason category, the routing stage, and the stale session_id.
	rejected404 := extractMatchingLines(log2Text, "request completed", "status=404")
	if len(rejected404) == 0 {
		t.Fatalf("server #2 never issued a 404 for the stale session id.\n--- server #2 log ---\n%s", log2Text)
	}
	established := extractMatchingLines(log2Text, "mcp session established", "")
	if len(established) == 0 {
		t.Fatalf("server #2 never logged a re-initialized session.\n--- server #2 log ---\n%s", log2Text)
	}
	respSent := extractMatchingLines(log2Text, "mcp response sent", "")

	// Surface the verbatim restart-moment log lines for the completion report.
	t.Logf("\n=== server #2 debug log around the restart moment ===\n"+
		"--- 404 for the stale session id (the spec-mandated stale-session response) ---\n%s\n"+
		"--- re-initialize (fresh session assigned) ---\n%s\n"+
		"--- successful recovery tool-call response(s) ---\n%s",
		strings.Join(rejected404, "\n"),
		strings.Join(established, "\n"),
		strings.Join(lastN(respSent, 3), "\n"))

	return true // scenario completed; all assertions ran
}

// extractMatchingLines returns every line containing both substrings (the second
// is ignored when empty).
func extractMatchingLines(text, must, also string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, must) && (also == "" || strings.Contains(ln, also)) {
			out = append(out, ln)
		}
	}
	return out
}

func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
