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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startProc starts the built binary with cfgPath, sending stdout+stderr to
// logPath, and waits until mcpPort accepts connections. It returns the running
// command and an open log file (the caller closes both via stopProc).
func startProc(t *testing.T, cfgPath, logPath string, mcpPort int) (*exec.Cmd, *os.File) {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("startProc: create log: %v", err)
	}
	cmd := exec.Command(builtServerBin, "-config", cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("startProc: start: %v", err)
	}
	if !waitMCPPort(mcpPort, 10*time.Second) {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
		t.Fatalf("startProc: MCP port %d never came up (see %s)", mcpPort, logPath)
	}
	return cmd, logFile
}

// stopProc sends SIGTERM, waits for the process to exit (so the listener is
// released before the next start reuses the port), and closes the log file.
func stopProc(t *testing.T, cmd *exec.Cmd, logFile *os.File) {
	t.Helper()
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	logFile.Close()
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
	// Reserve the ports ONCE; both server processes bind the same MCP port so the
	// stale client reconnects to the new process exactly as Claude Code would.
	httpPort := freePort(t)
	mcpPort := freePort(t)
	baseDir := t.TempDir() // shared data dir so the tool catalog is identical across restarts
	cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", false, "")
	mcpURL := mcpEndpoint(mcpPort)

	logPath1 := filepath.Join(t.TempDir(), "server1.log")
	logPath2 := filepath.Join(t.TempDir(), "server2.log")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Step 1: server #1 up, stale client connects and a tool works. ---
	cmd1, log1 := startProc(t, cfgPath, logPath1, mcpPort)
	t.Logf("server #1 started (pid %d), log: %s", cmd1.Process.Pid, logPath1)

	// DisableStandaloneSSE keeps the stale client to request/response POSTs only,
	// so after the restart its tool-call POST deterministically hits process #2
	// and gets the 404 — no background reconnect noise to race with.
	staleTransport := &mcp.StreamableClientTransport{Endpoint: mcpURL, DisableStandaloneSSE: true}
	staleCli := mcp.NewClient(&mcp.Implementation{Name: "restart-stale-client", Version: "0.0.1"}, nil)
	staleSess, err := staleCli.Connect(ctx, staleTransport, nil)
	if err != nil {
		stopProc(t, cmd1, log1)
		t.Fatalf("stale client Connect to server #1 failed: %v", err)
	}
	probeTool := callFirstNoArgTool(ctx, t, staleSess)
	t.Logf("server #1: tool %q succeeded on the stale session", probeTool)

	// --- Step 2: kill server #1, wait for exit (releases the port). ---
	stopProc(t, cmd1, log1)
	t.Logf("server #1 terminated")

	// --- Step 3: server #2 up on the SAME port (fresh, empty session map). ---
	cmd2, log2 := startProc(t, cfgPath, logPath2, mcpPort)
	defer stopProc(t, cmd2, log2)
	t.Logf("server #2 started (pid %d) on the same port, log: %s", cmd2.Process.Pid, logPath2)

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

	rejected404 := extractMatchingLines(log2Text, "request rejected", "status=404")
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
