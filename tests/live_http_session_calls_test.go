package tests

// Scenario A: drive ONE client/session through several tools/list + CallTool
// round-trips without disconnecting, mirroring the dogfooding flow
// (get_server_info → list_projects → ...) that issues multiple CallTools on a
// single session. The interop test only issues one CallTool before closing; this
// closes that coverage gap over the Streamable HTTP transport.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveHTTPSessionCalls(t *testing.T) {
	// Parameterized on log level like the interop test, so the debug run produces
	// a server-side log the completion report can include.
	for _, level := range []string{"info", "debug"} {
		level := level
		t.Run(level, func(t *testing.T) {
			baseDir := t.TempDir()
			var mcpPort int
			var logPath string
			cleanup := startLiveServer(t, func() liveLaunch {
				httpPort := freePort(t)
				mcpPort = freePort(t)
				cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, level, false, "")
				logPath = filepath.Join(t.TempDir(), "server.log")
				return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, mcpPort}}
			})
			defer cleanup()
			t.Logf("server log (level=%s): %s", level, logPath)

			mcpURL := mcpEndpoint(mcpPort)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			// One client, one session for the whole sequence.
			sess := connectClient(ctx, t, mcpURL, nil, "session-calls-test")
			defer sess.Close()

			noArg := discoverNoArgTools(ctx, t, sess)
			if len(noArg) == 0 {
				t.Fatalf("no no-argument tool discovered; cannot exercise sequential "+
					"calls on one session (this is itself a finding). server log: %s", logPath)
			}

			// Build the call sequence: if there are at least 3 no-arg tools, call
			// each once (>=3 sequential calls on one session); otherwise call each at
			// least twice (still exercises the session state machine across multiple
			// sequential requests).
			var sequence []string
			if len(noArg) >= 3 {
				sequence = append(sequence, noArg...)
			} else {
				for _, n := range noArg {
					sequence = append(sequence, n, n)
				}
			}
			t.Logf("discovered %d no-arg tool(s); issuing %d sequential CallTool(s) on one session",
				len(noArg), len(sequence))

			for i, name := range sequence {
				res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name})
				if err != nil {
					t.Fatalf("sequential CallTool #%d (%q) errored: %v\nserver log: %s",
						i, name, err, logPath)
				}
				if res.IsError {
					t.Fatalf("sequential CallTool #%d (%q) returned IsError=true: %s\nserver log: %s",
						i, name, wireText(res), logPath)
				}
				if len(res.Content) == 0 {
					t.Fatalf("sequential CallTool #%d (%q) returned empty content\nserver log: %s",
						i, name, logPath)
				}
			}
			t.Logf("completed %d sequential CallTool round-trips on one session without disconnecting",
				len(sequence))
		})
	}
}
