package tests

// Scenario C: capture the verbatim initialize handshake over the Streamable HTTP
// transport. The test starts the live binary at DEBUG, performs ONE Connect →
// Close cycle, then reads the server's debug log and extracts the two protocol
// lines:
//
//	"mcp message received" + rpc_method=initialize   (the client's request)
//	"mcp response sent"     with the matching rpc_id  (the server's response)
//
// The test asserts ONLY that both lines exist; it makes no assertion about their
// contents. The extracted rpc_params (request) and event_data (response) are
// saved to a file and logged so the completion report can quote them verbatim.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// extractSlogAttr pulls the value of a slog text-handler attribute (key=value)
// from one formatted log line. slog quotes values containing spaces or special
// characters (so JSON payloads arrive quoted); a quoted value is unquoted back to
// its original form. ok is false only if the key is absent.
func extractSlogAttr(line, key string) (value string, ok bool) {
	marker := key + "="
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(marker):]
	if rest == "" {
		return "", true
	}
	if rest[0] == '"' {
		for j := 1; j < len(rest); j++ {
			if rest[j] == '\\' { // skip the escaped character
				j++
				continue
			}
			if rest[j] == '"' {
				if v, err := strconv.Unquote(rest[:j+1]); err == nil {
					return v, true
				}
				return rest[:j+1], true
			}
		}
		return rest, true // unterminated quote: return as-is
	}
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		return rest[:end], true
	}
	return strings.TrimRight(rest, "\r\n"), true
}

func TestLiveHTTPInitializeCapture(t *testing.T) {
	httpPort := freePort(t)
	mcpPort := freePort(t)
	baseDir := t.TempDir()
	cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", false, "")
	logPath := filepath.Join(t.TempDir(), "server.log")
	cleanup := startLiveServer(t, cfgPath, logPath, mcpPort)
	defer cleanup()
	t.Logf("server debug log: %s", logPath)

	mcpURL := mcpEndpoint(mcpPort)

	// Single Connect → Close cycle. Connect performs the initialize handshake
	// synchronously, so by the time it returns the server has written both the
	// request (POST handler) and the response (POST response stream) log lines.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sess := connectClient(ctx, t, mcpURL, nil, "initialize-capture")
		sess.Close()
	}()

	// Poll the log file until both lines appear (they may be written by different
	// server goroutines), or the deadline passes.
	var reqLine, respLine string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("read server log %s: %v", logPath, err)
		}
		lines := strings.Split(string(data), "\n")

		reqLine = ""
		for _, ln := range lines {
			if strings.Contains(ln, "mcp message received") && strings.Contains(ln, "rpc_method=initialize") {
				reqLine = ln
				break
			}
		}
		if reqLine == "" {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		// Match the response to the request id exactly (avoids substring
		// collisions like rpc_id=1 vs rpc_id=10).
		wantID, _ := extractSlogAttr(reqLine, "rpc_id")
		respLine = ""
		for _, ln := range lines {
			if !strings.Contains(ln, "mcp response sent") {
				continue
			}
			gotID, _ := extractSlogAttr(ln, "rpc_id")
			if wantID == "" || gotID == wantID {
				respLine = ln
				break
			}
		}
		if respLine != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if reqLine == "" {
		t.Fatalf("initialize request line ('mcp message received' + rpc_method=initialize) "+
			"not found in server debug log %s", logPath)
	}
	if respLine == "" {
		t.Fatalf("initialize response line ('mcp response sent' with matching rpc_id) "+
			"not found in server debug log %s", logPath)
	}

	rpcParams, _ := extractSlogAttr(reqLine, "rpc_params")
	eventData, _ := extractSlogAttr(respLine, "event_data")

	// Persist the verbatim lines and the unquoted attribute values so the
	// completion report can quote them. Also log the content directly: t.TempDir()
	// is removed when the test ends, so the -v test output is the durable capture
	// the report is built from.
	capture := strings.Join([]string{
		"=== initialize request log line (verbatim) ===",
		reqLine,
		"",
		"=== initialize response log line (verbatim) ===",
		respLine,
		"",
		"=== initialize request rpc_params (unquoted JSON) ===",
		rpcParams,
		"",
		"=== initialize response event_data (unquoted JSON) ===",
		eventData,
		"",
	}, "\n")

	capturePath := filepath.Join(t.TempDir(), "initialize_capture.txt")
	if err := os.WriteFile(capturePath, []byte(capture), 0o600); err != nil {
		t.Fatalf("write capture file: %v", err)
	}
	t.Logf("initialize handshake capture written to: %s", capturePath)
	t.Logf("\n%s", capture)
}
