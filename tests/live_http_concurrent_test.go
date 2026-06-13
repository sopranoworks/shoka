package tests

// Scenario B: concurrent traffic against Shoka over the Streamable HTTP
// transport. The interop test issues requests sequentially from one goroutine;
// this exercises parallel traffic in two shapes, under `go test -race`:
//
//	B.1  N clients each running an independent full lifecycle concurrently.
//	B.2  N concurrent CallTools issued on a SINGLE shared session.
//
// SDK concurrency observation (B.2): the SDK's ClientSession is safe for
// concurrent use. Its requests funnel through internal/jsonrpc2.Connection,
// whose Call allocates request ids via atomic.AddInt64 and registers the
// outstanding call under a mutex, and whose writer is documented safe for
// concurrent use. Therefore B.2 drives concurrency directly on one ClientSession
// rather than at the raw HTTP level.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// concurrentClients is the N for both sub-scenarios (N=5).
const concurrentClients = 5

// oneClientLifecycle performs a full Connect → ListTools → CallTool(no-arg) →
// Close against mcpURL and returns an error instead of calling t.Fatal, because
// it runs on a goroutine other than the test goroutine (where t.Fatal is
// illegal). It mirrors runLifecycle from live_http_interop_test.go.
func oneClientLifecycle(mcpURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	transport := &mcp.StreamableClientTransport{Endpoint: mcpURL}
	client := mcp.NewClient(&mcp.Implementation{Name: "concurrent-client", Version: "0.0.1"}, nil)
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer sess.Close()

	lt, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	var picked string
	for _, tool := range lt.Tools {
		if hasNoRequiredArgs(tool.InputSchema) {
			picked = tool.Name
			break
		}
	}
	if picked == "" {
		return fmt.Errorf("no no-argument tool available among %d", len(lt.Tools))
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: picked})
	if err != nil {
		return fmt.Errorf("call %q: %w", picked, err)
	}
	if res.IsError {
		return fmt.Errorf("call %q: IsError=true: %s", picked, wireText(res))
	}
	if len(res.Content) == 0 {
		return fmt.Errorf("call %q: empty content", picked)
	}
	return nil
}

func TestLiveHTTPConcurrent(t *testing.T) {
	baseDir := t.TempDir()
	var mcpPort int
	var logPath string
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort := freePort(t)
		mcpPort = freePort(t)
		cfgPath := writeLiveConfig(t, baseDir, httpPort, mcpPort, "debug", false, "")
		logPath = filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, mcpPort}}
	})
	defer cleanup()
	t.Logf("server log: %s", logPath)

	mcpURL := mcpEndpoint(mcpPort)

	// B.1 — N clients each running a full lifecycle concurrently against the
	// same server process.
	t.Run("concurrent_clients", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]error, concurrentClients)
		for i := 0; i < concurrentClients; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = oneClientLifecycle(mcpURL) // each goroutine writes its own slot
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("concurrent client #%d failed: %v\nserver log: %s", i, err, logPath)
			}
		}
	})

	// B.2 — N concurrent CallTools on a single shared session. Different
	// goroutines may hit the same tool name; that is intentional.
	t.Run("concurrent_calls_one_session", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sess := connectClient(ctx, t, mcpURL, nil, "concurrent-one-session")
		defer sess.Close()

		noArg := discoverNoArgTools(ctx, t, sess)
		if len(noArg) == 0 {
			t.Fatalf("no no-argument tool discovered; cannot exercise concurrent "+
				"calls on one session. server log: %s", logPath)
		}

		var wg sync.WaitGroup
		errs := make([]error, concurrentClients)
		for i := 0; i < concurrentClients; i++ {
			i := i
			name := noArg[i%len(noArg)]
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name})
				if err != nil {
					errs[i] = fmt.Errorf("CallTool(%q): %w", name, err)
					return
				}
				if res.IsError {
					errs[i] = fmt.Errorf("CallTool(%q): IsError=true: %s", name, wireText(res))
					return
				}
				if len(res.Content) == 0 {
					errs[i] = fmt.Errorf("CallTool(%q): empty content", name)
				}
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("concurrent call #%d failed: %v\nserver log: %s", i, err, logPath)
			}
		}
	})
}
